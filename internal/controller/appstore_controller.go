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
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	appCatalogueName = "default"
)

// +kubebuilder:rbac:groups=gentianos.io,resources=appcatalogues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=appcatalogues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=appprofiles,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch

// AppStoreReconciler watches AppProfile and Tenant CRs and maintains the singleton
// AppCatalogue CR so that CLIs and UIs can discover available apps.
type AppStoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the AppStore controller.
func (r *AppStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToSingleton := func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: appCatalogueName}},
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.AppCatalogue{}).
		Watches(
			&gentianov1alpha1.AppProfile{},
			handler.EnqueueRequestsFromMapFunc(mapToSingleton),
		).
		Watches(
			&gentianov1alpha1.Tenant{},
			handler.EnqueueRequestsFromMapFunc(mapToSingleton),
		).
		Complete(r)
}

// Reconcile rebuilds the AppCatalogue status from AppProfile CRs in the cluster.
func (r *AppStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if req.Name != appCatalogueName {
		return ctrl.Result{}, nil
	}

	profileList := &gentianov1alpha1.AppProfileList{}
	if err := r.List(ctx, profileList); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}

	for i := range profileList.Items {
		p := &profileList.Items[i]
		if err := r.ensureProfileCatalogueLabels(ctx, p); err != nil {
			logger.Error(err, "failed to set profile catalogue labels", "profile", p.Name)
		}
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}
	installedCounts := buildInstalledCounts(tenantList.Items, profileList.Items)
	entries := buildProfileEntries(profileList.Items, installedCounts)

	now := metav1.NewTime(time.Now())

	catalogue := &gentianov1alpha1.AppCatalogue{}
	err := r.Get(ctx, types.NamespacedName{Name: appCatalogueName}, catalogue)
	if errors.IsNotFound(err) {
		catalogue = &gentianov1alpha1.AppCatalogue{
			ObjectMeta: metav1.ObjectMeta{Name: appCatalogueName},
		}
		if createErr := r.Create(ctx, catalogue); createErr != nil {
			reconcileErrors.WithLabelValues("appstore").Inc()
			return ctrl.Result{}, createErr
		}
		logger.Info("created AppCatalogue singleton")
	} else if err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(catalogue.DeepCopy())
	catalogue.Status.Apps = entries
	catalogue.Status.TotalApps = len(entries)
	catalogue.Status.LastUpdated = &now

	if err := r.Status().Patch(ctx, catalogue, patch); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}

	logger.Info("reconciled AppCatalogue", "apps", len(entries))
	return ctrl.Result{}, nil
}

func (r *AppStoreReconciler) ensureProfileCatalogueLabels(ctx context.Context, p *gentianov1alpha1.AppProfile) error {
	want := gentianov1alpha1.ProfileCatalogueLabels(p)
	if labelsMatch(p.Labels, want) {
		return nil
	}
	patched := p.DeepCopy()
	if patched.Labels == nil {
		patched.Labels = make(map[string]string)
	}
	for k, v := range want {
		patched.Labels[k] = v
	}
	return r.Patch(ctx, patched, client.MergeFrom(p))
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func buildProfileEntries(profiles []gentianov1alpha1.AppProfile, installedCounts map[string]int) []gentianov1alpha1.CatalogueEntry {
	entries := make([]gentianov1alpha1.CatalogueEntry, 0, len(profiles))
	for i := range profiles {
		p := &profiles[i]
		id := gentianov1alpha1.ProfileIdentityFor(p)
		entries = append(entries, gentianov1alpha1.CatalogueEntry{
			Name:               p.Name,
			Family:             id.Family,
			CatalogueVersion:   id.CatalogueVersion,
			Edition:            id.Edition,
			TrustTier:          gentianov1alpha1.EffectiveTrustTier(p.Spec.TrustTier),
			License:            p.Spec.License,
			DisplayName:        p.Spec.DisplayName,
			Description:        p.Spec.Description,
			ChartVersion:       p.Spec.Chart.Version,
			DeploymentMethod:   p.Spec.DeploymentMethod,
			KernelRequirements: kernelRequirementLabels(p.Spec.KernelRequirements),
			InstalledCount:     installedCounts[p.Name],
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Family != entries[j].Family {
			return entries[i].Family < entries[j].Family
		}
		if entries[i].CatalogueVersion != entries[j].CatalogueVersion {
			return entries[i].CatalogueVersion < entries[j].CatalogueVersion
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func buildInstalledCounts(tenants []gentianov1alpha1.Tenant, profiles []gentianov1alpha1.AppProfile) map[string]int {
	counts := make(map[string]int)
	for i := range tenants {
		for _, app := range tenants[i].Spec.Apps {
			name := app.Profile
			if name == "" && app.ProfileRef != nil {
				if resolved, ok := gentianov1alpha1.ResolveProfileReference(profiles, *app.ProfileRef); ok {
					name = resolved
				}
			}
			if name != "" {
				counts[name]++
			}
		}
	}
	return counts
}

func kernelRequirementLabels(kr *gentianov1alpha1.KernelRequirements) []string {
	if kr == nil {
		return nil
	}
	set := make(map[string]struct{})
	if kr.Identity != nil {
		if kr.Identity.OIDC != nil {
			set["oidc"] = struct{}{}
		}
	}
	if kr.Database != nil {
		set[string(kr.Database.Engine)] = struct{}{}
	}
	if kr.Storage != nil {
		if kr.Storage.S3 != nil {
			set["s3"] = struct{}{}
		}
		if kr.Storage.Files != nil {
			set["files"] = struct{}{}
		}
	}
	if kr.Cache != nil {
		set[string(kr.Cache.Engine)] = struct{}{}
	}
	if kr.Mail != nil {
		if kr.Mail.SMTP != nil {
			set["smtp"] = struct{}{}
		}
		if kr.Mail.IMAP != nil {
			set["imap"] = struct{}{}
		}
	}
	if kr.MCP != nil {
		set["mcp"] = struct{}{}
	}

	labels := make([]string, 0, len(set))
	for k := range set {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	return labels
}
