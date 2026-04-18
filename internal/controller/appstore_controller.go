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

// AppStoreReconciler watches AppProfile CRs and maintains the singleton
// AppCatalogue CR so that CLIs and UIs can discover available apps without
// listing raw AppProfile objects.
//
// It also watches Tenant CRs to keep the InstalledCount cross-reference
// up to date whenever tenants add or remove apps.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=appcatalogues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=appcatalogues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=appprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch
type AppStoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the AppStore controller. It reconciles whenever
// an AppProfile or Tenant changes, because both events affect the catalogue status.
func (r *AppStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Any AppProfile or Tenant change should trigger a full catalogue rebuild.
	// We map these events to a fixed reconcile request for the singleton AppCatalogue.
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

// Reconcile rebuilds the AppCatalogue status from all AppProfile CRs in the cluster.
func (r *AppStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Only process the singleton.
	if req.Name != appCatalogueName {
		return ctrl.Result{}, nil
	}

	// List all AppProfiles.
	profileList := &gentianov1alpha1.AppProfileList{}
	if err := r.List(ctx, profileList); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}

	// Count how many tenants reference each AppProfile.
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		reconcileErrors.WithLabelValues("appstore").Inc()
		return ctrl.Result{}, err
	}
	installedCounts := buildInstalledCounts(tenantList.Items)

	// Build catalogue entries.
	entries := make([]gentianov1alpha1.CatalogueEntry, 0, len(profileList.Items))
	for i := range profileList.Items {
		p := &profileList.Items[i]
		entries = append(entries, gentianov1alpha1.CatalogueEntry{
			Name:               p.Name,
			DisplayName:        p.Spec.DisplayName,
			Description:        p.Spec.Description,
			ChartVersion:       p.Spec.Chart.Version,
			DeploymentMethod:   p.Spec.DeploymentMethod,
			KernelRequirements: kernelRequirementLabels(p.Spec.KernelRequirements),
			InstalledCount:     installedCounts[p.Name],
		})
	}
	// Stable ordering so the status diff is deterministic.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	now := metav1.NewTime(time.Now())

	// Fetch or create the singleton AppCatalogue CR.
	catalogue := &gentianov1alpha1.AppCatalogue{}
	err := r.Get(ctx, types.NamespacedName{Name: appCatalogueName}, catalogue)
	if errors.IsNotFound(err) {
		catalogue = &gentianov1alpha1.AppCatalogue{
			ObjectMeta: metav1.ObjectMeta{
				Name: appCatalogueName,
			},
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

	// Patch status.
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

// buildInstalledCounts returns a map from AppProfile name → number of Tenants
// that currently list it in their spec.apps.
func buildInstalledCounts(tenants []gentianov1alpha1.Tenant) map[string]int {
	counts := make(map[string]int)
	for i := range tenants {
		for _, app := range tenants[i].Spec.Apps {
			counts[app.Profile]++
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
		if kr.Identity.OIDC {
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
