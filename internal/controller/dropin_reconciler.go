/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/customization"
)

const (
	conditionDropInsReady = "DropInsReady"

	// dropInConfigMapPrefix namespaces generated drop-in ConfigMaps so they are
	// distinguishable from app-owned config.
	dropInConfigMapPrefix = "app-dropin"

	// dropInLabel marks generated ConfigMaps for cleanup of removed drop-ins.
	dropInLabel = "gentianos.io/dropin"
	// dropInProfileLabel records which profile a drop-in belongs to.
	dropInProfileLabel = "gentianos.io/profile-name"
)

// dropInFileNamePattern reserves the 90-99 numeric prefix range for tenants, so
// platform (00-49) and profile (50-89) drop-ins keep priority in the lexicographic
// ordering the app applies them in.
var dropInFileNamePattern = regexp.MustCompile(`^9[0-9]-[a-zA-Z0-9._-]+$`)

// ensureTenantDropIns materialises tenant-authored L1 drop-in content into
// ConfigMaps in the tenant namespace, one per (profile, drop-in) pair.
//
// This is rung L1 at tenant scope (see docs/app-customization.md §2.2.1): the
// highest rung a tenant admin may reach unaided. A tenant may supply content —
// a logo, a locale bundle, a config fragment — into a directory the AppProfile
// has explicitly declared as tenantEditable. It may not introduce code, invent
// mount paths, or shadow platform files.
//
// The mount itself is generated from AppProfile.spec.customization.dropIns by the
// composition, so no per-app operator logic is involved: the operator only writes
// validated content into a predictably named ConfigMap.
func (r *TenantReconciler) ensureTenantDropIns(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	desired, err := r.buildTenantDropIns(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionDropInsReady, metav1.ConditionFalse, "InvalidDropIn", err.Error())
		// Invalid tenant content is a spec error, not a transient failure — surface
		// it on the Tenant and stop rather than hot-looping.
		return ctrl.Result{}, nil
	}

	if err := r.applyDropInConfigMaps(ctx, tenant, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply drop-in ConfigMaps: %w", err)
	}

	if err := r.pruneDropInConfigMaps(ctx, tenant, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("prune drop-in ConfigMaps: %w", err)
	}

	if len(desired) == 0 {
		r.setCondition(tenant, conditionDropInsReady, metav1.ConditionTrue,
			"NoDropIns", "No tenant drop-ins configured")
		return ctrl.Result{}, nil
	}
	r.setCondition(tenant, conditionDropInsReady, metav1.ConditionTrue,
		"DropInsApplied", fmt.Sprintf("%d tenant drop-in ConfigMap(s) reconciled", len(desired)))
	return ctrl.Result{}, nil
}

// dropInConfigMapSpec is one ConfigMap to be written for a tenant drop-in.
type dropInConfigMapSpec struct {
	name    string
	profile string
	dropIn  string
	data    map[string]string
}

// buildTenantDropIns validates every tenant drop-in against the declaration on the
// target AppProfile and returns the ConfigMaps to write. Validation is total: any
// invalid entry fails the whole build, so a tenant cannot half-apply a change.
func (r *TenantReconciler) buildTenantDropIns(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
) ([]dropInConfigMapSpec, error) {
	var specs []dropInConfigMapSpec
	if len(tenant.Spec.Apps) == 0 {
		return specs, nil
	}

	profileIndex, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, fmt.Errorf("load AppProfile index: %w", err)
	}

	for _, app := range tenant.Spec.Apps {
		if app.Config == nil || len(app.Config.DropIns) == 0 {
			continue
		}
		profile, exists := appProfileFromIndex(profileIndex, app.Profile)
		if !exists {
			return nil, fmt.Errorf("app %q: AppProfile not found", app.Profile)
		}

		for _, tenantDropIn := range app.Config.DropIns {
			declared := customization.FindDropIn(profile.Spec.Customization, tenantDropIn.Name)
			if declared == nil {
				return nil, fmt.Errorf(
					"app %q: drop-in %q is not declared in spec.customization.dropIns — "+
						"tenants cannot invent mount paths",
					app.Profile, tenantDropIn.Name)
			}
			if !declared.TenantEditable {
				return nil, fmt.Errorf(
					"app %q: drop-in %q is not tenantEditable",
					app.Profile, tenantDropIn.Name)
			}
			if err := validateDropInFiles(declared, tenantDropIn.Files); err != nil {
				return nil, fmt.Errorf("app %q: drop-in %q: %w", app.Profile, tenantDropIn.Name, err)
			}

			specs = append(specs, dropInConfigMapSpec{
				name:    dropInConfigMapName(app.Profile, tenantDropIn.Name),
				profile: app.Profile,
				dropIn:  tenantDropIn.Name,
				data:    tenantDropIn.Files,
			})
		}
	}

	sort.Slice(specs, func(i, j int) bool { return specs[i].name < specs[j].name })
	return specs, nil
}

// validateDropInFiles enforces the §2.2.1 guardrails: reserved filename range,
// size cap, and parseability of the declared format. Content is parsed here so a
// malformed fragment fails reconciliation rather than crashing the app at boot.
func validateDropInFiles(declared *gentianov1alpha1.CustomizationDropIn, files map[string]string) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	total := 0
	for _, name := range names {
		if !dropInFileNamePattern.MatchString(name) {
			return fmt.Errorf(
				"file %q must match 9X-<name> — the 00-89 prefixes are reserved for platform and profile files",
				name)
		}
		total += len(name) + len(files[name])
	}

	maxBytes := int(declared.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = customization.DefaultDropInMaxBytes
	}
	if total > maxBytes {
		return fmt.Errorf("content is %d bytes, exceeding the %d byte limit", total, maxBytes)
	}

	for _, name := range names {
		if err := customization.ValidateDropInContent(declared.Format, files[name]); err != nil {
			return fmt.Errorf("file %q: %w", name, err)
		}
	}
	return nil
}

// dropInConfigMapName is the predictable name the composition mounts.
func dropInConfigMapName(profile, dropIn string) string {
	return fmt.Sprintf("%s-%s-%s", dropInConfigMapPrefix, profile, dropIn)
}

func (r *TenantReconciler) applyDropInConfigMaps(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	desired []dropInConfigMapSpec,
) error {
	ns := tenantNamespaceName(tenant)
	for _, spec := range desired {
		cm := &corev1.ConfigMap{}
		err := r.Get(ctx, types.NamespacedName{Name: spec.name, Namespace: ns}, cm)
		if errors.IsNotFound(err) {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      spec.name,
					Namespace: ns,
					Labels: map[string]string{
						managedByLabel:     managedByValue,
						dropInLabel:        spec.dropIn,
						dropInProfileLabel: spec.profile,
					},
				},
				Data: spec.data,
			}
			if err := r.Create(ctx, cm); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[managedByLabel] = managedByValue
		cm.Labels[dropInLabel] = spec.dropIn
		cm.Labels[dropInProfileLabel] = spec.profile
		cm.Data = spec.data
		if err := r.Update(ctx, cm); err != nil {
			return err
		}
	}
	return nil
}

// pruneDropInConfigMaps deletes generated ConfigMaps whose drop-in has been removed
// from the Tenant. Without this a removed customization would keep applying — the
// tenant would have no way to undo it short of a cluster hotfix.
func (r *TenantReconciler) pruneDropInConfigMaps(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	desired []dropInConfigMapSpec,
) error {
	ns := tenantNamespaceName(tenant)
	var existing corev1.ConfigMapList
	if err := r.List(ctx, &existing,
		client.InNamespace(ns),
		client.HasLabels{dropInLabel},
	); err != nil {
		return err
	}

	keep := make(map[string]struct{}, len(desired))
	for _, spec := range desired {
		keep[spec.name] = struct{}{}
	}

	for i := range existing.Items {
		cm := &existing.Items[i]
		if !strings.HasPrefix(cm.Name, dropInConfigMapPrefix+"-") {
			continue
		}
		if _, ok := keep[cm.Name]; ok {
			continue
		}
		if err := r.Delete(ctx, cm); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
