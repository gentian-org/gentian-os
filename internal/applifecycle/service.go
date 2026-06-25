/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing limitations under the License.
*/

package applifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/gentian-org/gentian-os/internal/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const platformAppAnnotation = "gentianos.io/platform-app"

var appClaimGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "App",
}

// Service implements tenant app install/uninstall/purge.
type Service struct {
	client    client.Client
	clientset kubernetes.Interface
	opts      Options
	git       *GitOps
}

// NewService constructs a lifecycle service.
func NewService(c client.Client, cfg *rest.Config, opts Options) (*Service, error) {
	if opts.KernelNamespace == "" {
		opts.KernelNamespace = meta.KernelNamespace
	}
	if opts.OpenBaoNamespace == "" {
		opts.OpenBaoNamespace = "openbao"
	}
	if opts.OperatorNamespace == "" {
		opts.OperatorNamespace = "gentian-system"
	}
	if opts.OperatorSA == "" {
		opts.OperatorSA = "gentian-os"
	}
	if opts.DefaultBackend == "" {
		opts.DefaultBackend = BackendKubernetes
	}
	if opts.WaitTimeout == 0 {
		opts.WaitTimeout = 15 * time.Minute
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Service{
		client:    c,
		clientset: cs,
		opts:      opts,
		git:       NewGitOps(opts.DeploymentsPath, opts.DeploymentsRepo),
	}, nil
}

func (s *Service) backend(reqBackend Backend) Backend {
	if reqBackend != "" {
		return reqBackend
	}
	return s.opts.DefaultBackend
}

// Install adds a profile to the tenant and waits until the App claim is Ready.
func (s *Service) Install(ctx context.Context, req InstallRequest) (*Result, error) {
	if err := s.validateProfile(ctx, req.Profile); err != nil {
		return nil, err
	}
	backend := s.backend(req.Backend)
	status, err := s.addProfile(ctx, backend, req.Tenant, req.Profile, req.Actor)
	if err != nil {
		return nil, err
	}
	if status == "already_installed" {
		ready, msg, err := s.appReadyState(ctx, req.Tenant, req.Profile)
		if err != nil {
			return nil, err
		}
		return &Result{
			Status:  status,
			Tenant:  req.Tenant,
			Profile: req.Profile,
			Backend: backend,
			Ready:   ready,
			Message: msg,
		}, nil
	}
	if err := s.waitForAppReady(ctx, req.Tenant, req.Profile, s.opts.WaitTimeout); err != nil {
		return nil, err
	}
	return &Result{
		Status:  status,
		Tenant:  req.Tenant,
		Profile: req.Profile,
		Backend: backend,
		Ready:   true,
		Message: "Installed and ready",
	}, nil
}

// Uninstall removes a profile from the tenant and optionally purges persistent state.
func (s *Service) Uninstall(ctx context.Context, req UninstallRequest) (*Result, error) {
	backend := s.backend(req.Backend)
	tenant := &gentianov1alpha1.Tenant{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: req.Tenant}, tenant); err != nil {
		return nil, fmt.Errorf("get tenant %q: %w", req.Tenant, err)
	}
	profileCR := &gentianov1alpha1.AppProfile{}
	profileErr := s.client.Get(ctx, client.ObjectKey{Name: req.Profile}, profileCR)
	if profileErr != nil {
		if apierrors.IsNotFound(profileErr) {
			profileCR = nil
		} else {
			return nil, fmt.Errorf("get appprofile %q: %w", req.Profile, profileErr)
		}
	}

	status, err := s.removeProfile(ctx, backend, req.Tenant, req.Profile, req.Actor)
	if err != nil {
		return nil, err
	}
	if status == "not_installed" && !s.appClaimExists(ctx, req.Tenant, req.Profile) {
		return &Result{Status: status, Tenant: req.Tenant, Profile: req.Profile, Backend: backend}, nil
	}

	if err := s.deleteAppClaim(ctx, req.Tenant, req.Profile); err != nil {
		return nil, err
	}
	if err := s.waitForAppUninstalled(ctx, req.Tenant, req.Profile, s.opts.WaitTimeout); err != nil {
		return nil, err
	}

	var warnings []string
	if req.Purge {
		warnings = s.purge(ctx, tenant, profileCR, req.Profile)
	} else if req.Profile == "element" && profileUsesPostgres(profileCR) {
		warnings = append(warnings, s.cleanupElementSynapseIdentities(ctx, req.Tenant)...)
	}

	return &Result{
		Status:   status,
		Tenant:   req.Tenant,
		Profile:  req.Profile,
		Backend:  backend,
		Purged:   req.Purge,
		Warnings: warnings,
	}, nil
}

func (s *Service) validateProfile(ctx context.Context, profile string) error {
	ap := &gentianov1alpha1.AppProfile{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: profile}, ap); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("appprofile %q not found", profile)
		}
		return err
	}
	if ap.Annotations != nil && ap.Annotations[platformAppAnnotation] == "true" {
		return fmt.Errorf("cannot modify platform app %q", profile)
	}
	return nil
}

func profileUsesPostgres(ap *gentianov1alpha1.AppProfile) bool {
	if ap == nil || ap.Spec.KernelRequirements == nil || ap.Spec.KernelRequirements.Database == nil {
		return false
	}
	return ap.Spec.KernelRequirements.Database.Engine == gentianov1alpha1.DatabaseEnginePostgreSQL
}

func (s *Service) addProfile(ctx context.Context, backend Backend, tenant, profile, actor string) (string, error) {
	switch backend {
	case BackendGitOps:
		return s.git.Install(tenant, profile, actor)
	default:
		return s.patchTenantAddApp(ctx, tenant, profile)
	}
}

func (s *Service) removeProfile(ctx context.Context, backend Backend, tenant, profile, actor string) (string, error) {
	switch backend {
	case BackendGitOps:
		return s.git.Uninstall(tenant, profile, actor)
	default:
		return s.patchTenantRemoveApp(ctx, tenant, profile)
	}
}

func (s *Service) patchTenantAddApp(ctx context.Context, tenantName, profile string) (string, error) {
	tenant := &gentianov1alpha1.Tenant{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: tenantName}, tenant); err != nil {
		return "", err
	}
	for _, app := range tenant.Spec.Apps {
		if app.Profile == profile {
			return "already_installed", nil
		}
	}
	patch := client.MergeFrom(tenant.DeepCopy())
	tenant.Spec.Apps = append(tenant.Spec.Apps, gentianov1alpha1.TenantApp{Profile: profile})
	if err := s.client.Patch(ctx, tenant, patch); err != nil {
		return "", err
	}
	return "installed", nil
}

func (s *Service) patchTenantRemoveApp(ctx context.Context, tenantName, profile string) (string, error) {
	tenant := &gentianov1alpha1.Tenant{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: tenantName}, tenant); err != nil {
		return "", err
	}
	next := make([]gentianov1alpha1.TenantApp, 0, len(tenant.Spec.Apps))
	found := false
	for _, app := range tenant.Spec.Apps {
		if app.Profile == profile {
			found = true
			continue
		}
		next = append(next, app)
	}
	if !found {
		return "not_installed", nil
	}
	patch := client.MergeFrom(tenant.DeepCopy())
	tenant.Spec.Apps = next
	if err := s.client.Patch(ctx, tenant, patch); err != nil {
		return "", err
	}
	return "uninstalled", nil
}

func (s *Service) appClaimExists(ctx context.Context, tenant, profile string) bool {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	err := s.client.Get(ctx, client.ObjectKey{Name: profile, Namespace: tenantNamespace(tenant)}, obj)
	return err == nil
}

func (s *Service) deleteAppClaim(ctx context.Context, tenant, profile string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	key := client.ObjectKey{Name: profile, Namespace: tenantNamespace(tenant)}
	if err := s.client.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return client.IgnoreNotFound(s.client.Delete(ctx, obj))
}

func (s *Service) appReadyState(ctx context.Context, tenant, profile string) (bool, string, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	if err := s.client.Get(ctx, client.ObjectKey{Name: profile, Namespace: tenantNamespace(tenant)}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "Install requested — waiting for the app claim to be created", nil
		}
		return false, "", err
	}
	ready, msg := claimReady(obj)
	return ready, msg, nil
}

func claimReady(obj *unstructured.Unstructured) (bool, string) {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false, "Provisioning in progress"
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "Ready" && cond["status"] == "True" {
			return true, "Installed and ready"
		}
		if cond["type"] == "Ready" && cond["message"] != nil {
			return false, fmt.Sprint(cond["message"])
		}
	}
	return false, "Provisioning in progress"
}

// ListInstalled returns non-platform apps from tenant.spec.apps with claim status.
func (s *Service) ListInstalled(ctx context.Context, tenant string) ([]Result, error) {
	t := &gentianov1alpha1.Tenant{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: tenant}, t); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(t.Spec.Apps))
	for _, app := range t.Spec.Apps {
		if app.Profile == "" {
			continue
		}
		ap := &gentianov1alpha1.AppProfile{}
		if err := s.client.Get(ctx, client.ObjectKey{Name: app.Profile}, ap); err == nil {
			if ap.Annotations != nil && ap.Annotations[platformAppAnnotation] == "true" {
				continue
			}
		}
		ready, msg, err := s.appReadyState(ctx, tenant, app.Profile)
		if err != nil {
			return nil, err
		}
		out = append(out, Result{
			Tenant:  tenant,
			Profile: app.Profile,
			Ready:   ready,
			Message: msg,
		})
	}
	return out, nil
}
