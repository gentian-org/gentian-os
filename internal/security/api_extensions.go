/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security

import (
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The API extensions of a component.
//
// A profile may bring resource types of its own (requires.privileges.
// apiExtensions): CustomResourceDefinitions, written out in the profile so
// that what break-glass approves is what is installed. This file decides
// whether a request's definitions are ones the platform installs at all; the
// operator installs them once a live grant is on the install, and the
// admission guard (charts/gentian-os/templates/api-extensions-guard.yaml)
// holds the operator's own writes to the same rules.

// APIExtensionLabel is on every CustomResourceDefinition the operator
// installed for a profile, with the profile's name. A definition without it,
// or with another profile's, is never changed by the operator.
const APIExtensionLabel = "gentianos.io/api-extension"

// APIExtensionsClusterRole is the one ClusterRole through which components
// read their types: get, list and watch on every installed type, and its
// status written. The operator keeps its rules to the installed types and
// binds it to a component's ServiceAccount while the grant lives. One role
// rather than one per profile, because the operator may change this one by
// name and no other (escalate is held to it).
const APIExtensionsClusterRole = "gentian-api-extensions"

// reservedAPIGroups are the groups, and the domains of groups, no extension
// may be in: Kubernetes' own and the platform's. The profile's schema refuses
// them as well; this is the check the operator acts on.
var reservedAPIGroups = []string{"k8s.io", "kubernetes.io", "gentianos.io"}

// ReservedAPIGroup reports whether group is one an extension may not be in.
func ReservedAPIGroup(group string) bool {
	if !strings.Contains(group, ".") {
		return true
	}
	for _, reserved := range reservedAPIGroups {
		if group == reserved || strings.HasSuffix(group, "."+reserved) {
			return true
		}
	}
	return false
}

// APIExtensionDefinitions reads the definitions of one request and checks
// each is one the platform installs: a CustomResourceDefinition of
// apiextensions.k8s.io/v1, in the request's group, named for its plural,
// namespaced, with no conversion webhook -- the API server would call the component on every
// read of the type -- and none named twice. What is returned is the
// definition as it is installed: its name and spec, and nothing else of the
// profile's metadata.
func APIExtensionDefinitions(req *gentianov1alpha1.APIExtensionRequest) ([]*apiextensionsv1.CustomResourceDefinition, error) {
	if ReservedAPIGroup(req.Group) {
		return nil, fmt.Errorf("group %s is Kubernetes' or the platform's, not the add-on's own", req.Group)
	}
	seen := map[string]bool{}
	out := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(req.Definitions))
	for i := range req.Definitions {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(req.Definitions[i].Raw, &crd); err != nil {
			return nil, fmt.Errorf("definition %d is not a CustomResourceDefinition: %w", i+1, err)
		}
		switch {
		case crd.APIVersion != apiextensionsv1.SchemeGroupVersion.String() || crd.Kind != "CustomResourceDefinition":
			return nil, fmt.Errorf("definition %d is a %s %s, not an apiextensions.k8s.io/v1 CustomResourceDefinition", i+1, crd.APIVersion, crd.Kind)
		case crd.Spec.Group != req.Group:
			return nil, fmt.Errorf("%s is in group %s, not %s", crd.Name, crd.Spec.Group, req.Group)
		case crd.Spec.Names.Plural == "" || crd.Name != crd.Spec.Names.Plural+"."+req.Group:
			return nil, fmt.Errorf("%s is not named <plural>.%s", crd.Name, req.Group)
		case crd.Spec.Scope != apiextensionsv1.NamespaceScoped:
			// An object of the type lives in a tenant's namespace, or the
			// platform tenant's for what is the cluster's, and is declared
			// in that tenant's directory, which Argo CD prunes.
			return nil, fmt.Errorf("%s is not namespaced; an add-on's objects are a tenant's", crd.Name)
		case crd.Spec.Conversion != nil && crd.Spec.Conversion.Strategy != apiextensionsv1.NoneConverter:
			return nil, fmt.Errorf("%s has a conversion webhook, which the API server would call on every read", crd.Name)
		case seen[crd.Name]:
			return nil, fmt.Errorf("%s is defined twice", crd.Name)
		}
		seen[crd.Name] = true
		out = append(out, &apiextensionsv1.CustomResourceDefinition{
			TypeMeta:   crd.TypeMeta,
			ObjectMeta: metav1.ObjectMeta{Name: crd.Name},
			Spec:       crd.Spec,
		})
	}
	return out, nil
}
