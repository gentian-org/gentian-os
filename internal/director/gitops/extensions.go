/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// An add-on's object, as a commit.
//
// An add-on brings types of its own (requires.privileges.apiExtensions), and
// what is declared in them -- a tenant's backup schedule, say -- is declared
// the way everything else of a tenant is: a file in its directory, with an
// author, applied by Argo CD into the tenant's namespace and pruned when the
// file goes. What is the cluster's rather than one tenant's is declared in the
// platform tenant's directory the same way. The add-on reads the objects and
// does not write them.
//
//	clusters/<c>/tenants/<t>/ext-<resource>.<group>-<name>.yaml
//
// Which types there are is not this file's to know: the director asks the
// cluster for the ones installed and granted, and checks the object against
// its type before it is written here.

// ExtensionObject is one object of an add-on's type.
type ExtensionObject struct {
	Group    string
	Version  string
	Kind     string
	Resource string
	Name     string
	Spec     map[string]any
}

// ErrInvalidExtensionObject is an object that cannot be written as given.
var ErrInvalidExtensionObject = errors.New("invalid add-on object")

// ErrExtensionObjectNotFound is an object nothing declares.
var ErrExtensionObjectNotFound = errors.New("no such add-on object")

var (
	extensionGroup    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)
	extensionResource = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	extensionVersion  = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]+)?$`)
	extensionKind     = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
)

// CheckExtensionSpec holds an object's spec to its type's schema (the
// version's openAPIV3Schema, as the cluster has it), so that what is
// committed is what the API server will take: a refused object would fail
// the sync of the tenant's whole directory. The schema's CEL rules are the
// API server's alone, and are met there.
func CheckExtensionSpec(schema json.RawMessage, spec map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	var v1 apiextensionsv1.JSONSchemaProps
	if err := json.Unmarshal(schema, &v1); err != nil {
		return fmt.Errorf("read the type's schema: %w", err)
	}
	specSchema, ok := v1.Properties["spec"]
	if !ok {
		return nil
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&specSchema, &internal, nil); err != nil {
		return fmt.Errorf("read the type's schema: %w", err)
	}
	sv, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		return fmt.Errorf("read the type's schema: %w", err)
	}
	var problems []string
	for _, e := range validation.ValidateCustomResource(field.NewPath("spec"), spec, sv) {
		problems = append(problems, e.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidExtensionObject, strings.Join(problems, "; "))
	}
	return nil
}

// extensionFile is the file an object is declared in: its type and its name,
// neither of which can hold a slash.
func extensionFile(group, resource, name string) (string, error) {
	if !extensionGroup.MatchString(group) || len(group) > 200 || !extensionResource.MatchString(resource) || len(resource) > 63 {
		return "", fmt.Errorf("%w: %s.%s is not a type's name", ErrInvalidExtensionObject, resource, group)
	}
	if !ValidName(name) {
		return "", fmt.Errorf("%w: an object's name is lower-case letters, digits and hyphens, 63 at most", ErrInvalidExtensionObject)
	}
	return "ext-" + resource + "." + group + "-" + name + ".yaml", nil
}

// DeclareExtensionObject writes one object into a tenant's directory and
// commits it.
func (g *GitOps) DeclareExtensionObject(ctx context.Context, tenant string, o ExtensionObject, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	file, err := extensionFile(o.Group, o.Resource, o.Name)
	if err != nil {
		return Result{}, err
	}
	if !extensionVersion.MatchString(o.Version) || !extensionKind.MatchString(o.Kind) {
		return Result{}, fmt.Errorf("%w: %s/%s %s", ErrInvalidExtensionObject, o.Group, o.Version, o.Kind)
	}
	spec := o.Spec
	if spec == nil {
		spec = map[string]any{}
	}
	body, err := yaml.Marshal(map[string]any{
		"apiVersion": o.Group + "/" + o.Version,
		"kind":       o.Kind,
		"metadata":   map[string]any{"name": o.Name, "namespace": layout.Tenant(tenant)},
		"spec":       spec,
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidExtensionObject, err)
	}
	header := "# Managed by the director: an object of an add-on's own type, declared by\n" +
		"# whoever the commit names. The add-on reads it and does not write it.\n"
	return g.writeTenantFile(ctx, tenant, file, header+string(body), listResource,
		fmt.Sprintf("Declare %s %s of tenant %s", o.Kind, o.Name, tenant), meta)
}

// RemoveExtensionObject stops declaring one. The tenant's directory is
// synced with pruning, so the cluster deletes it.
func (g *GitOps) RemoveExtensionObject(ctx context.Context, tenant, group, resource, name string, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	file, err := extensionFile(group, resource, name)
	if err != nil {
		return Result{}, err
	}
	res, err := g.writeTenantFile(ctx, tenant, file, "", listResource,
		fmt.Sprintf("Remove %s.%s %s of tenant %s", resource, group, name, tenant), meta)
	if err == nil && res.Status == "unchanged" {
		return Result{}, ErrExtensionObjectNotFound
	}
	return res, err
}

// PlatformTenant names the platform's own tenant, which declares what of an
// add-on is the cluster's rather than one tenant's.
func (g *GitOps) PlatformTenant(ctx context.Context) (string, error) {
	tenants, err := g.Tenants(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range tenants {
		platform, err := g.IsPlatformTenant(ctx, t)
		if err != nil {
			return "", err
		}
		if platform {
			return t, nil
		}
	}
	return "", ErrTenantNotFound
}
