/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// What the registrar needs to know about the cluster, and where it reads it.
//
// The director answered these from git, because git is what the director
// holds. The registrar holds no git credential, so it reads the objects the
// operator reconciles: the Tenants, and the Cluster claim. Read-only, and
// those two kinds only.

// ErrTenantNotFound is a tenant this cluster does not have.
var ErrTenantNotFound = errors.New("tenant not found")

// platformTenant is the tenant whose realm is the kernel realm: its people
// are the platform's own, and its groups are gentian:platform:*.
const platformTenant = "platform"

// defaultAdminGroup is the group the Cluster claim's schema names as the
// platform's administrators when the claim says nothing else.
const defaultAdminGroup = "gentian:platform:admin"

// adminRole is the field of spec.platformRoles that default belongs to.
const adminRole = "admin"

// Tenant is what the routes need to know about one tenant.
type Tenant struct {
	Name string
	// Realm is the Keycloak realm its people live in.
	Realm string
	// LoginDomain is the domain its people sign in under: the part after the
	// @ in a login composed from a local part, and in its administrator's
	// login. Empty when it cannot be told yet.
	LoginDomain string
	// AdminRequiresMFA says whether activating its administrator account
	// includes enrolling a second factor.
	AdminRequiresMFA bool
}

// Tenants answers what the cluster says about its tenants.
type Tenants interface {
	// Tenant is one tenant, or ErrTenantNotFound.
	Tenant(ctx context.Context, name string) (Tenant, error)
	// All is every tenant of the cluster.
	All(ctx context.Context) ([]Tenant, error)
}

// ClusterTenants reads the Tenant objects.
type ClusterTenants struct {
	Client client.Reader
	// KernelDomain is the cluster's own domain, which a tenant that adopts
	// another realm signs in under.
	KernelDomain string
}

// Tenant reads one Tenant object.
func (c *ClusterTenants) Tenant(ctx context.Context, name string) (Tenant, error) {
	if !dnsLabel.MatchString(name) {
		return Tenant{}, fmt.Errorf("%w: tenant %q", errInvalidName, name)
	}
	var t gentianov1alpha1.Tenant
	if err := c.Client.Get(ctx, types.NamespacedName{Name: name}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			return Tenant{}, fmt.Errorf("%w: %q", ErrTenantNotFound, name)
		}
		return Tenant{}, fmt.Errorf("reading tenant %q: %w", name, err)
	}
	return c.facts(&t), nil
}

// All lists the Tenant objects, by name.
func (c *ClusterTenants) All(ctx context.Context) ([]Tenant, error) {
	var list gentianov1alpha1.TenantList
	if err := c.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}
	out := make([]Tenant, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, c.facts(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *ClusterTenants) facts(t *gentianov1alpha1.Tenant) Tenant {
	// The realm a tenant names, or the one of its own name: the same default
	// the operator applies when it writes the registrar's credential per
	// realm (keycloakRealmName) and the custodian applies in the other
	// direction (tenantOfRealm). The three must agree, or this asks for a
	// credential nobody wrote.
	realm := t.Name
	if t.Spec.Isolation != nil && t.Spec.Isolation.KeycloakRealm != "" {
		realm = t.Spec.Isolation.KeycloakRealm
	}
	return Tenant{
		Name:             t.Name,
		Realm:            realm,
		LoginDomain:      c.loginDomain(t, realm),
		AdminRequiresMFA: t.AdminRequiresMFA(),
	}
}

// loginDomain is the domain a tenant's people sign in under.
//
// The rule is the director's (gitops.TenantLoginDomain), read from where the
// cluster keeps the same facts:
//
//   - the custom domain a TenantDomain binds, which the operator copies to
//     the tenant's status once it has accepted it;
//   - the kernel domain for a tenant that adopts another realm -- the
//     platform tenant, whose people are the kernel realm's (admin@<kernel>,
//     not admin@platform.<kernel>);
//   - otherwise the domain the operator itself resolved for the tenant,
//     which is <tenant>.<kernel>, or <kernel> for the user tenant of a
//     single-tenancy cluster. It is
//     taken from the administrator address the operator reports on the
//     tenant's status rather than derived again here, so the tenancy mode is
//     decided once, by the operator, from the claim.
//
// Empty for a tenant the operator has not reconciled yet; a caller that
// needs the domain says so rather than guessing one.
func (c *ClusterTenants) loginDomain(t *gentianov1alpha1.Tenant, realm string) string {
	kernel := strings.ToLower(strings.TrimSpace(c.KernelDomain))
	if custom := strings.ToLower(strings.TrimSpace(t.Status.Domain)); custom != "" &&
		custom != kernel && !strings.HasSuffix(custom, "."+kernel) {
		return custom
	}
	if realm != t.Name {
		return kernel
	}
	_, domain, ok := strings.Cut(t.Status.AdminEmail, "@")
	// <tenant>.invalid is what the operator reports when it knows no domain.
	if !ok || domain == "" || strings.HasSuffix(domain, ".invalid") {
		return ""
	}
	return domain
}

// clusterClaimList is the Cluster claim, read unstructured: nothing registers
// that kind with the scheme, and the registrar needs one string from it.
var clusterClaimList = schema.GroupVersionKind{Group: "gentianos.io", Version: "v1alpha1", Kind: "ClusterList"}

// ClaimPlatformRoleGroups answers which groups' members hold a platform role:
// every field of spec.platformRoles of the Cluster claim.
//
// The same map, from the same object, in the same namespace as the operator's
// projection reads (AuthzProjectionReconciler.platformRoles): the groups the
// registrar refuses to touch have to be the groups the operator projects as
// the cluster's roles, and two readings of two sources would eventually
// differ. The map is read whole rather than field by field, so a role the
// schema gains is protected without this function learning its name.
//
// The one default is the schema's own: a claim that names no administrators'
// group leaves gentian:platform:admin, which is also where the install puts
// the first administrator. No other role has a default, and a role the claim
// leaves unset has no group to protect.
//
// An error is an error: the caller refuses the write rather than assume a
// name. That includes a platformRoles that is not a map of strings, which the
// schema does not admit and which could only be read in part.
func ClaimPlatformRoleGroups(reader client.Reader) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(clusterClaimList)
		if err := reader.List(ctx, list, client.InNamespace(layout.Namespace(layout.Provisioning))); err != nil {
			return nil, fmt.Errorf("reading the Cluster claim: %w", err)
		}
		seen := map[string]bool{}
		var out []string
		add := func(group string) {
			group = strings.TrimPrefix(strings.TrimSpace(group), "/")
			if group != "" && !seen[group] {
				seen[group] = true
				out = append(out, group)
			}
		}
		for i := range list.Items {
			roles, _, err := unstructured.NestedStringMap(list.Items[i].Object, "spec", "platformRoles")
			if err != nil {
				return nil, fmt.Errorf("reading spec.platformRoles of the Cluster claim %s: %w", list.Items[i].GetName(), err)
			}
			for _, group := range roles {
				add(group)
			}
			if strings.TrimSpace(roles[adminRole]) == "" {
				add(defaultAdminGroup)
			}
		}
		if len(out) == 0 {
			out = []string{defaultAdminGroup}
		}
		sort.Strings(out)
		return out, nil
	}
}
