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

// TenantSyncFailed is a tenant this cluster does not have because Argo CD's
// last sync of its Application failed: a tenant the deployments repository
// declares and the cluster did not create, rather than one on its way. It is
// still ErrTenantNotFound, so whatever waits for a tenant goes on waiting --
// a sync asked again can still bring it.
type TenantSyncFailed struct {
	Name string
	// Message is Argo CD's, as the Application reports it.
	Message string
}

func (e *TenantSyncFailed) Error() string {
	return fmt.Sprintf("the cluster did not create tenant %q: the last sync of its Argo CD Application %s failed: %s",
		e.Name, layout.Tenant(e.Name), e.Message)
}

func (e *TenantSyncFailed) Unwrap() error { return ErrTenantNotFound }

// platformTenant is the tenant whose realm is the kernel realm: its people
// are the platform's own.
const platformTenant = "platform"

// Tenant is what the routes need to know about one tenant.
type Tenant struct {
	Name string
	// DisplayName is what a person called it; empty falls back to the name.
	DisplayName string
	// Realm is the Keycloak realm its people live in.
	Realm string
	// LoginDomain is the domain its people sign in under: the part after the
	// @ in a login composed from a local part, and in its administrator's
	// login. Empty when it cannot be told yet.
	LoginDomain string
	// AdminLogin is the login of its administrator account, the one
	// activate-admin hands over. Not always admin@<LoginDomain>: the user
	// tenant of a single-tenancy cluster lives on the cluster's own domain,
	// where admin@ is the platform administrator, and its administrator is
	// user-admin@. Empty when it cannot be told yet.
	AdminLogin string
	// AdminRequiresMFA says whether activating its administrator account
	// includes enrolling a second factor.
	AdminRequiresMFA bool
	// SignUp says people make their own accounts in its realm, where the
	// address of an account is its login.
	SignUp bool
	// MailboxDomain is the domain its people have mailboxes under on the
	// cluster's own mail server, as the operator reports it on the tenant.
	// Empty when they have none: removing a person then asks nothing about
	// their mail.
	MailboxDomain string
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
	// Applications reads a tenant's Argo CD Application (tenant-<name>, in
	// ApplicationNamespace) when the Tenant is not there, to tell a failed
	// creation from one on its way. Read unstructured, which a manager's
	// client answers from the API server and not from a cache: one get, no
	// watch. Nil reads none, and every missing tenant is on its way.
	Applications         client.Reader
	ApplicationNamespace string
}

var applicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// syncFailure is Argo CD's message when the last sync of the tenant's
// Application failed and it is not in sync, or empty: no Application, one
// that cannot be read, one still syncing or retrying, or one in sync.
func (c *ClusterTenants) syncFailure(ctx context.Context, name string) string {
	if c.Applications == nil || c.ApplicationNamespace == "" {
		return ""
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	if err := c.Applications.Get(ctx, types.NamespacedName{Namespace: c.ApplicationNamespace, Name: layout.Tenant(name)}, app); err != nil {
		return ""
	}
	phase, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "phase")
	sync, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
	if (phase != "Failed" && phase != "Error") || sync == "Synced" {
		return ""
	}
	msg, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "message")
	if msg == "" {
		msg = "Argo CD gave no message"
	}
	return msg
}

// Tenant reads one Tenant object.
func (c *ClusterTenants) Tenant(ctx context.Context, name string) (Tenant, error) {
	if !gentianov1alpha1.ValidTenantName(name) {
		return Tenant{}, fmt.Errorf("%w: tenant %q", errInvalidName, name)
	}
	var t gentianov1alpha1.Tenant
	if err := c.Client.Get(ctx, types.NamespacedName{Name: name}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			if msg := c.syncFailure(ctx, name); msg != "" {
				return Tenant{}, &TenantSyncFailed{Name: name, Message: msg}
			}
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
	domain := c.loginDomain(t, realm)
	return Tenant{
		Name:             t.Name,
		DisplayName:      t.Spec.DisplayName,
		Realm:            realm,
		LoginDomain:      domain,
		AdminLogin:       adminLogin(t, realm, domain),
		AdminRequiresMFA: t.AdminRequiresMFA(),
		SignUp:           t.SignUpEnabled(),
		MailboxDomain:    strings.ToLower(strings.TrimSpace(t.Status.MailboxDomain)),
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

// adminLogin is the login of the tenant's administrator account.
//
// The operator creates that account and reports its login on the tenant's
// status (the address is the username: Tenant.TenantAdminUsername), so for a
// tenant with a realm of its own it is read from there and not composed
// again. Composed here as admin@<domain> it was wrong for the one tenant
// whose administrator is not called admin -- the user tenant of a
// single-tenancy cluster, user-admin@<kernel> -- and activate-admin looked
// for an account that does not exist and answered "still being provisioned"
// for as long as it was asked.
//
// A tenant that adopts another realm keeps admin@<domain>: that is the
// platform tenant, and the account is the kernel realm's, which the operator
// reports under the tenant's own name instead. And where the reported
// address is not under the login domain -- a custom domain the status has
// not caught up with -- the domain the people sign in under wins.
func adminLogin(t *gentianov1alpha1.Tenant, realm, domain string) string {
	if domain == "" {
		return ""
	}
	reported := strings.ToLower(strings.TrimSpace(t.Status.AdminEmail))
	if realm == t.Name && strings.HasSuffix(reported, "@"+domain) {
		return reported
	}
	return gentianov1alpha1.TenantAdminLocalPart + "@" + domain
}

// clusterClaimList is the Cluster claim, read unstructured: nothing registers
// that kind with the scheme, and the registrar needs one string from it.
var clusterClaimList = schema.GroupVersionKind{Group: "gentianos.io", Version: "v1alpha1", Kind: "ClusterList"}

// RoleHolder is one entry of the Cluster claim's
// spec.platformAccess.roleHolders: a person who holds a cluster role by
// name, with their account in their tenant's realm.
type RoleHolder struct {
	// Role is the role as the claim spells it: admin, securityOfficer,
	// auditor, breakGlass and the others.
	Role string `json:"role"`
	// Tenant is the tenant whose person holds it; platform for a person of
	// the kernel realm.
	Tenant string `json:"tenant"`
	// Subject is the account's id in that tenant's realm.
	Subject string `json:"subject"`
	// Username and Email say who it is to a person reading the claim.
	Username string `json:"username,omitempty"`
	Email    string `json:"email,omitempty"`
}

// ClaimRoleHolders answers who holds a cluster role by name: every entry of
// spec.platformAccess.roleHolders of the Cluster claim, read from the same
// object in the same namespace as the operator's projection reads, on every
// call. Sorted by role, tenant and username.
//
// An error is an error: a caller that judges a write by the answer refuses
// the write rather than assume nobody holds a role.
func ClaimRoleHolders(reader client.Reader) func(ctx context.Context) ([]RoleHolder, error) {
	return func(ctx context.Context) ([]RoleHolder, error) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(clusterClaimList)
		if err := reader.List(ctx, list, client.InNamespace(layout.Namespace(layout.Provisioning))); err != nil {
			return nil, fmt.Errorf("reading the Cluster claim: %w", err)
		}
		out := []RoleHolder{}
		for i := range list.Items {
			entries, _, err := unstructured.NestedSlice(list.Items[i].Object, "spec", "platformAccess", "roleHolders")
			if err != nil {
				return nil, fmt.Errorf("reading spec.platformAccess.roleHolders of the Cluster claim %s: %w", list.Items[i].GetName(), err)
			}
			for _, e := range entries {
				m, _ := e.(map[string]interface{})
				h := RoleHolder{}
				h.Role, _ = m["role"].(string)
				h.Tenant, _ = m["tenant"].(string)
				h.Subject, _ = m["subject"].(string)
				h.Username, _ = m["username"].(string)
				h.Email, _ = m["email"].(string)
				if h.Role != "" && h.Subject != "" {
					out = append(out, h)
				}
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if a.Role != b.Role {
				return a.Role < b.Role
			}
			if a.Tenant != b.Tenant {
				return a.Tenant < b.Tenant
			}
			return a.Username < b.Username
		})
		return out, nil
	}
}

// ClaimPlatformRoleHolders answers the subjects of the platform tenant's
// people the Cluster claim names role holders, the cluster's administrators
// among them: the kernel-realm accounts the registrar does not write to
// (identity/guard.go). Read from the claim on every write.
func ClaimPlatformRoleHolders(reader client.Reader) func(ctx context.Context) ([]string, error) {
	holders := ClaimRoleHolders(reader)
	return func(ctx context.Context) ([]string, error) {
		held, err := holders(ctx)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		var out []string
		for _, h := range held {
			if h.Tenant == platformTenant && !seen[h.Subject] {
				seen[h.Subject] = true
				out = append(out, h.Subject)
			}
		}
		return out, nil
	}
}
