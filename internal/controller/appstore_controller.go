/*
Copyright 2026 The Gentian Authors.

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
	// appCatalogueName is the fixed name of the singleton AppCatalogue CR.
	appCatalogueName = "default"
)

// AppStoreReconciler watches AppProfile, AppProduct, and Tenant CRs and maintains
// the singleton AppCatalogue CR so that CLIs and UIs can discover available apps
// without listing raw catalogue objects.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=appcatalogues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=appcatalogues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=appprofiles,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=appproducts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch
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
			&gentianov1alpha1.AppProduct{},
			handler.EnqueueRequestsFromMapFunc(mapToSingleton),
		).
		Watches(
			&gentianov1alpha1.Tenant{},
			handler.EnqueueRequestsFromMapFunc(mapToSingleton),
		).
		Complete(r)
}

// Reconcile rebuilds the AppCatalogue status from catalogue CRs in the cluster.
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

	productList := &gentianov1alpha1.AppProductList{}
	if err := r.List(ctx, productList); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}

	for i := range productList.Items {
		prod := &productList.Items[i]
		if err := r.ensureProductNameLabel(ctx, prod); err != nil {
			logger.Error(err, "failed to set product-name label", "product", prod.Name)
		}
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}
	installedCounts := buildInstalledCounts(tenantList.Items, profileList.Items)

	profileEntries := buildProfileEntries(profileList.Items, installedCounts)
	productEntries := buildProductEntries(productList.Items, profileList.Items, tenantList.Items)

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
	catalogue.Status.Apps = profileEntries
	catalogue.Status.TotalApps = len(profileEntries)
	catalogue.Status.Products = productEntries
	catalogue.Status.TotalProducts = len(productEntries)
	catalogue.Status.LastUpdated = &now

	if err := r.Status().Patch(ctx, catalogue, patch); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}

	logger.Info("reconciled AppCatalogue", "apps", len(profileEntries), "products", len(productEntries))
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

func (r *AppStoreReconciler) ensureProductNameLabel(ctx context.Context, prod *gentianov1alpha1.AppProduct) error {
	if prod.Labels[gentianov1alpha1.LabelProductName] == prod.Name {
		return nil
	}
	patched := prod.DeepCopy()
	if patched.Labels == nil {
		patched.Labels = make(map[string]string)
	}
	patched.Labels[gentianov1alpha1.LabelProductName] = prod.Name
	return r.Patch(ctx, patched, client.MergeFrom(prod))
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
			OfferingTier:       id.OfferingTier,
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

func buildProductEntries(
	products []gentianov1alpha1.AppProduct,
	profiles []gentianov1alpha1.AppProfile,
	tenants []gentianov1alpha1.Tenant,
) []gentianov1alpha1.ProductEntry {
	entries := make([]gentianov1alpha1.ProductEntry, 0, len(products))
	for i := range products {
		prod := &products[i]
		resolved := resolveProductProfileRefs(prod.Spec.ProfileRefs, profiles)
		krSet := make(map[string]struct{})
		for _, pname := range resolved {
			for _, p := range profiles {
				if p.Name != pname {
					continue
				}
				for _, label := range kernelRequirementLabels(p.Spec.KernelRequirements) {
					krSet[label] = struct{}{}
				}
				break
			}
		}
		kr := make([]string, 0, len(krSet))
		for k := range krSet {
			kr = append(kr, k)
		}
		sort.Strings(kr)

		full, partial := productInstallCounts(resolved, tenants)
		entries = append(entries, gentianov1alpha1.ProductEntry{
			Name:                prod.Name,
			DisplayName:         prod.Spec.DisplayName,
			Description:         prod.Spec.Description,
			CatalogueVersion:    gentianov1alpha1.EffectiveCatalogueVersion(prod.Spec.CatalogueVersion),
			Edition:             gentianov1alpha1.EffectiveEdition(prod.Spec.Edition),
			OfferingTier:        gentianov1alpha1.EffectiveOfferingTier(prod.Spec.OfferingTier),
			TrustTier:           gentianov1alpha1.EffectiveTrustTier(prod.Spec.TrustTier),
			ProfileRefs:         resolved,
			ProfileCount:        len(resolved),
			Publisher:           prod.Spec.Publisher.Name,
			KernelRequirements:  kr,
			InstalledCount:      full,
			PartialInstallCount: partial,
			Listable:            gentianov1alpha1.EffectiveListable(prod.Spec.Listable),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func resolveProductProfileRefs(refs []gentianov1alpha1.ProfileReference, profiles []gentianov1alpha1.AppProfile) []string {
	names := make([]string, 0, len(refs))
	seen := make(map[string]struct{})
	for _, ref := range refs {
		name, ok := gentianov1alpha1.ResolveProfileReference(profiles, ref)
		if !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func productInstallCounts(profileNames []string, tenants []gentianov1alpha1.Tenant) (full, partial int) {
	if len(profileNames) == 0 {
		return 0, 0
	}
	want := make(map[string]struct{}, len(profileNames))
	for _, n := range profileNames {
		want[n] = struct{}{}
	}
	for i := range tenants {
		have := make(map[string]struct{})
		for _, app := range tenants[i].Spec.Apps {
			if app.Profile != "" {
				have[app.Profile] = struct{}{}
			}
		}
		matched := 0
		for n := range want {
			if _, ok := have[n]; ok {
				matched++
			}
		}
		switch {
		case matched == len(want):
			full++
		case matched > 0:
			partial++
		}
	}
	return full, partial
}

// buildInstalledCounts returns a map from AppProfile name → number of Tenants
// that currently list it in their spec.apps.
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

// kernelRequirementLabels converts the structured KernelRequirements into a
// compact sorted list of human-readable labels (e.g. ["ldap", "oidc", "postgresql", "s3"]).
func kernelRequirementLabels(kr *gentianov1alpha1.KernelRequirements) []string {
	if kr == nil {
		return nil
	}
	set := make(map[string]struct{})
	if kr.Identity != nil {
		if kr.Identity.OIDC != nil {
			set["oidc"] = struct{}{}
		}
		if kr.Identity.LDAP != nil {
			set["ldap"] = struct{}{}
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
