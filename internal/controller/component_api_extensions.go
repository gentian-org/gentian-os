/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// The API extensions of a component.
//
// A profile's apiExtensions are resource types of its own, written out in the
// profile (internal/security/api_extensions.go). Once break-glass granted
// apiExtensions/<name> on the install, the operator installs the definitions
// -- as new types, in a group no other definition on the cluster is in -- and
// binds gentian-api-extensions to the ServiceAccount the request names, so
// that the component reads its types and writes their status. Nothing else
// writes an object of the types but the director, through git
// (internal/director/api/extensions.go).
//
// A grant that lapses unbinds the role, and the install waits for a new one.
// The types stay: removing a definition deletes every object of it, which is
// a decision a person makes, not something an expiry does.

// The markers are a free-floating block: controller-gen ignores a block that
// is part of a declaration's doc comment.
//
// Definitions are created and updated, never deleted. The admission guard
// (charts/gentian-os/templates/api-extensions-guard.yaml) holds the writes to
// the definitions of a profile, outside the platform's groups.
// They are read by name, through the API reader, and never listed: the
// group's other types come from discovery.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;create;update
//
// The one role is created empty and then given its rules, which escalate,
// held to its name, permits; its binding is held to it with bind.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=update;escalate;bind,resourceNames=gentian-api-extensions
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;delete

const (
	// conditionAPIExtensionsInstalled says, on a Component whose profile asks
	// for API extensions, which were installed and why each other was not.
	conditionAPIExtensionsInstalled = "APIExtensionsInstalled"

	reasonAPIExtensionsInstalled = "Installed"
	reasonAPIExtensionNotGranted = "NotGranted"
	reasonAPIExtensionInvalid    = "InvalidDefinition"
	reasonAPIExtensionGroupTaken = "GroupTaken"
	reasonAPIExtensionTypeTaken  = "TypeTaken"

	// A binding's component, under labels of its own so that the cluster
	// roles' bindings (component_cluster_roles.go) and these never take each
	// other for their own.
	apiExtensionNamespaceLabel = "gentianos.io/api-extension-namespace"
	apiExtensionComponentLabel = "gentianos.io/api-extension-component"
)

// apiExtensionVerdict is what became of one request.
type apiExtensionVerdict struct {
	name, serviceAccount string
	installed            bool
	reason, message      string
}

// ensureAPIExtensions installs what is granted, binds the reading role to
// whoever's grant lives and unbinds the rest, and writes the condition. It
// returns why the install must wait when a granted extension could not be
// installed: a component started without its types fails on its first read.
func (r *ComponentReconciler) ensureAPIExtensions(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) (string, error) {
	var requests []gentianov1alpha1.APIExtensionRequest
	if p := profile.Privileges(); p != nil {
		requests = p.APIExtensions
	}
	if len(requests) == 0 && !hadAPIExtensions(comp) {
		// Nothing asked for, now or before: nothing bound to remove, and
		// the API server is not asked.
		return "", nil
	}
	granted := security.LiveGrants(profile, comp.Spec.Privileges, time.Now())
	verdicts := make([]apiExtensionVerdict, 0, len(requests))
	for i := range requests {
		req := &requests[i]
		v := apiExtensionVerdict{name: req.Name, serviceAccount: req.ServiceAccount}
		if _, ok := granted[security.PrivilegeRef(security.PrivilegeAPIExtensions, req.Name)]; !ok {
			v.reason, v.message = reasonAPIExtensionNotGranted, req.Name+": not granted on this install, the grant has expired, or the definitions changed since they were approved"
			verdicts = append(verdicts, v)
			continue
		}
		reason, message, err := r.installAPIExtension(ctx, profile.Name, req)
		if err != nil {
			return "", err
		}
		v.installed, v.reason, v.message = reason == reasonAPIExtensionsInstalled, reason, message
		verdicts = append(verdicts, v)
	}
	if err := r.bindAPIExtensions(ctx, comp, verdicts); err != nil {
		return "", err
	}
	setAPIExtensionsCondition(comp, verdicts)
	for _, v := range verdicts {
		if v.reason != reasonAPIExtensionsInstalled && v.reason != reasonAPIExtensionNotGranted {
			return v.message, nil
		}
	}
	return "", nil
}

// installAPIExtension installs one granted request's definitions, or says
// why it does not.
func (r *ComponentReconciler) installAPIExtension(ctx context.Context, profile string, req *gentianov1alpha1.APIExtensionRequest) (string, string, error) {
	defs, err := security.APIExtensionDefinitions(req)
	if err != nil {
		return reasonAPIExtensionInvalid, req.Name + ": " + err.Error(), nil
	}
	groupChecked := false
	for _, def := range defs {
		def.Labels = map[string]string{managedByLabel: managedByValue, security.APIExtensionLabel: profile}
		have := &apiextensionsv1.CustomResourceDefinition{}
		err := r.CRDReader.Get(ctx, client.ObjectKey{Name: def.Name}, have)
		if errors.IsNotFound(err) {
			// A new type: the group is the add-on's own, with no type of
			// another's in it -- a platform's, a kernel service's or
			// another add-on's. Asked once, and only when something is
			// added.
			if !groupChecked {
				taken, err := r.apiGroupTakenBy(ctx, req.Group, profile)
				if err != nil {
					return "", "", err
				}
				if taken != "" {
					return reasonAPIExtensionGroupTaken, fmt.Sprintf("%s: group %s already serves %s, which is not %s's", req.Name, req.Group, taken, profile), nil
				}
				groupChecked = true
			}
			if err := r.Create(ctx, def); err != nil && !errors.IsAlreadyExists(err) {
				return "", "", fmt.Errorf("create CustomResourceDefinition %s: %w", def.Name, err)
			}
			continue
		}
		if err != nil {
			return "", "", fmt.Errorf("get CustomResourceDefinition %s: %w", def.Name, err)
		}
		if have.Labels[security.APIExtensionLabel] != profile {
			return reasonAPIExtensionTypeTaken, fmt.Sprintf("%s: %s is on the cluster already, and is not %s's", req.Name, def.Name, profile), nil
		}
		if equality.Semantic.DeepEqual(have.Spec, def.Spec) {
			continue
		}
		have.Spec = def.Spec
		if err := r.Update(ctx, have); err != nil {
			return "", "", fmt.Errorf("update CustomResourceDefinition %s: %w", def.Name, err)
		}
	}
	return reasonAPIExtensionsInstalled, fmt.Sprintf("%s: %d type(s) of %s installed", req.Name, len(defs), req.Group), nil
}

// apiGroupTakenBy names a resource the API server serves in group that is
// not one of profile's installed types, or is empty when there is none. The
// operator lists no definitions (it may only get them by name), so the
// group's resources come from discovery, and each is looked up by the name
// its definition would have; one with no definition is served by something
// other than a definition, and is not the profile's either.
func (r *ComponentReconciler) apiGroupTakenBy(ctx context.Context, group, profile string) (string, error) {
	if r.Discovery == nil {
		return "", fmt.Errorf("no discovery client to tell whose group %s is", group)
	}
	groups, err := r.Discovery.ServerGroups()
	if err != nil {
		return "", fmt.Errorf("discover API groups: %w", err)
	}
	for i := range groups.Groups {
		if groups.Groups[i].Name != group {
			continue
		}
		for _, version := range groups.Groups[i].Versions {
			resources, err := r.Discovery.ServerResourcesForGroupVersion(version.GroupVersion)
			if err != nil && !errors.IsNotFound(err) {
				return "", fmt.Errorf("discover %s: %w", version.GroupVersion, err)
			}
			if resources == nil {
				continue
			}
			for _, resource := range resources.APIResources {
				if strings.Contains(resource.Name, "/") {
					continue
				}
				name := resource.Name + "." + group
				crd := &apiextensionsv1.CustomResourceDefinition{}
				if err := r.CRDReader.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
					if errors.IsNotFound(err) {
						return name, nil
					}
					return "", fmt.Errorf("get CustomResourceDefinition %s: %w", name, err)
				}
				if crd.Labels[security.APIExtensionLabel] != profile {
					return name, nil
				}
			}
		}
	}
	return "", nil
}

// apiExtensionBindingName is one binding's name, readable and unique to the
// component and its account.
func apiExtensionBindingName(namespace, component, serviceAccount string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + component + "\x00" + serviceAccount))
	readable := namespace + "-" + component
	if len(readable) > 180 {
		readable = readable[:180]
	}
	return security.APIExtensionsClusterRole + "-" + readable + "-" + hex.EncodeToString(sum[:5])
}

// bindAPIExtensions keeps the role's rules to the installed types, and its
// bindings for this component to the accounts of the installed requests.
func (r *ComponentReconciler) bindAPIExtensions(ctx context.Context, comp *gentianov1alpha1.Component, verdicts []apiExtensionVerdict) error {
	desired := map[string]*rbacv1.ClusterRoleBinding{}
	for _, v := range verdicts {
		if !v.installed {
			continue
		}
		name := apiExtensionBindingName(comp.Namespace, comp.Name, v.serviceAccount)
		desired[name] = &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					managedByLabel:             managedByValue,
					apiExtensionNamespaceLabel: comp.Namespace,
					apiExtensionComponentLabel: comp.Name,
				},
			},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: security.APIExtensionsClusterRole},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: v.serviceAccount, Namespace: comp.Namespace,
			}},
		}
	}
	if len(desired) > 0 {
		if err := r.ensureAPIExtensionsRole(ctx); err != nil {
			return err
		}
	}
	existing, err := r.apiExtensionBindings(ctx, comp)
	if err != nil {
		return err
	}
	for i := range existing {
		have := &existing[i]
		if want, keep := desired[have.Name]; keep && have.RoleRef == want.RoleRef &&
			len(have.Subjects) == 1 && have.Subjects[0] == want.Subjects[0] {
			delete(desired, have.Name)
			continue
		}
		if err := r.Delete(ctx, have); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ClusterRoleBinding %s: %w", have.Name, err)
		}
	}
	for _, binding := range desired {
		if err := r.Create(ctx, binding); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create ClusterRoleBinding %s: %w", binding.Name, err)
		}
	}
	return nil
}

// ensureAPIExtensionsRole keeps gentian-api-extensions to exactly the
// installed types: each read, and its status written. Which types are
// installed is read from the profiles that ask for them, each confirmed on
// the cluster by name, since the operator lists no definitions.
func (r *ComponentReconciler) ensureAPIExtensionsRole(ctx context.Context) error {
	profiles := &gentianov1alpha1.ComponentProfileList{}
	if err := r.List(ctx, profiles); err != nil {
		return fmt.Errorf("list ComponentProfiles: %w", err)
	}
	byGroup := map[string][]string{}
	for i := range profiles.Items {
		p := profiles.Items[i].Privileges()
		if p == nil {
			continue
		}
		for j := range p.APIExtensions {
			defs, err := security.APIExtensionDefinitions(&p.APIExtensions[j])
			if err != nil {
				continue
			}
			for _, def := range defs {
				crd := &apiextensionsv1.CustomResourceDefinition{}
				if err := r.CRDReader.Get(ctx, client.ObjectKey{Name: def.Name}, crd); err != nil {
					if errors.IsNotFound(err) {
						continue
					}
					return fmt.Errorf("get CustomResourceDefinition %s: %w", def.Name, err)
				}
				if crd.Labels[security.APIExtensionLabel] != profiles.Items[i].Name {
					continue
				}
				byGroup[crd.Spec.Group] = append(byGroup[crd.Spec.Group], crd.Spec.Names.Plural)
			}
		}
	}
	groups := make([]string, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	var rules []rbacv1.PolicyRule
	for _, g := range groups {
		plurals := byGroup[g]
		sort.Strings(plurals)
		status := make([]string, 0, len(plurals))
		for _, p := range plurals {
			status = append(status, p+"/status")
		}
		rules = append(rules,
			rbacv1.PolicyRule{APIGroups: []string{g}, Resources: plurals, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{g}, Resources: status, Verbs: []string{"get", "update", "patch"}},
		)
	}

	role := &rbacv1.ClusterRole{}
	err := r.Get(ctx, client.ObjectKey{Name: security.APIExtensionsClusterRole}, role)
	if errors.IsNotFound(err) {
		// Created without rules, which needs no permission the operator
		// lacks; the rules follow as an update, which escalate permits for
		// this one name.
		role = &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
			Name:   security.APIExtensionsClusterRole,
			Labels: map[string]string{managedByLabel: managedByValue},
		}}
		if err := r.Create(ctx, role); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create ClusterRole %s: %w", security.APIExtensionsClusterRole, err)
		}
		if err := r.Get(ctx, client.ObjectKey{Name: security.APIExtensionsClusterRole}, role); err != nil {
			return fmt.Errorf("get ClusterRole %s: %w", security.APIExtensionsClusterRole, err)
		}
	} else if err != nil {
		return fmt.Errorf("get ClusterRole %s: %w", security.APIExtensionsClusterRole, err)
	}
	if equality.Semantic.DeepEqual(role.Rules, rules) && role.AggregationRule == nil {
		return nil
	}
	role.Rules, role.AggregationRule = rules, nil
	if err := r.Update(ctx, role); err != nil {
		return fmt.Errorf("update ClusterRole %s: %w", security.APIExtensionsClusterRole, err)
	}
	return nil
}

// hadAPIExtensions reports whether the component asked for API extensions at
// its last reconcile: the condition stays until its bindings are gone.
func hadAPIExtensions(comp *gentianov1alpha1.Component) bool {
	return apimeta.FindStatusCondition(comp.Status.Conditions, conditionAPIExtensionsInstalled) != nil
}

// deleteAPIExtensionBindings removes every binding of a component that is
// going. Its types stay.
func (r *ComponentReconciler) deleteAPIExtensionBindings(ctx context.Context, comp *gentianov1alpha1.Component) error {
	if !hadAPIExtensions(comp) {
		return nil
	}
	existing, err := r.apiExtensionBindings(ctx, comp)
	if err != nil {
		return err
	}
	for i := range existing {
		if err := r.Delete(ctx, &existing[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ClusterRoleBinding %s: %w", existing[i].Name, err)
		}
	}
	return nil
}

func (r *ComponentReconciler) apiExtensionBindings(ctx context.Context, comp *gentianov1alpha1.Component) ([]rbacv1.ClusterRoleBinding, error) {
	list := &rbacv1.ClusterRoleBindingList{}
	if err := r.List(ctx, list, client.MatchingLabels{
		managedByLabel:             managedByValue,
		apiExtensionNamespaceLabel: comp.Namespace,
		apiExtensionComponentLabel: comp.Name,
	}); err != nil {
		return nil, fmt.Errorf("list API-extension bindings of %s/%s: %w", comp.Namespace, comp.Name, err)
	}
	return list.Items, nil
}

// setAPIExtensionsCondition says what became of each request. No request, no
// condition.
func setAPIExtensionsCondition(comp *gentianov1alpha1.Component, verdicts []apiExtensionVerdict) {
	if len(verdicts) == 0 {
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, conditionAPIExtensionsInstalled)
		return
	}
	messages := make([]string, 0, len(verdicts))
	reason := reasonAPIExtensionsInstalled
	for _, v := range verdicts {
		messages = append(messages, v.message)
		if !v.installed && reason == reasonAPIExtensionsInstalled {
			reason = v.reason
		}
	}
	sort.Strings(messages)
	status := metav1.ConditionTrue
	if reason != reasonAPIExtensionsInstalled {
		status = metav1.ConditionFalse
	}
	apimeta.SetStatusCondition(&comp.Status.Conditions, metav1.Condition{
		Type: conditionAPIExtensionsInstalled, Status: status, Reason: reason,
		Message: strings.Join(messages, "; "), ObservedGeneration: comp.Generation,
	})
}
