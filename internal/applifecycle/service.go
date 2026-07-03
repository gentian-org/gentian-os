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

package applifecycle

import (
	"context"
	"fmt"
	"time"

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

// Service implements tenant app install/uninstall/purge via GitOps.
type Service struct {
	client    client.Client
	clientset kubernetes.Interface
	opts      Options
	git       *GitOps
}

// NewService constructs a lifecycle service.
func NewService(c client.Client, cfg *rest.Config, opts Options) (*Service, error) {
	if opts.KernelNamespace == "" {
		opts.KernelNamespace = "platform-kernel"
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
		git:       NewGitOps(opts.DeploymentsPath, opts.DeploymentsRepo, opts.DeploymentsCluster, opts.DeploymentsStage),
	}, nil
}

// Install commits the profile to gentian-deployments, reconciles, and waits until Ready.
func (s *Service) Install(ctx context.Context, req InstallRequest) (*Result, error) {
	if err := s.validateProfile(ctx, req.Profile); err != nil {
		return nil, err
	}

	status, file, _, err := s.git.Install(req.Tenant, req.Profile, req.Actor)
	if err != nil {
		return nil, err
	}
	if status == "already_installed" {
		ready, msg, err := s.appReadyState(ctx, req.Tenant, req.Profile)
		if err != nil {
			return nil, err
		}
		if ready {
			return &Result{
				Status:  status,
				Tenant:  req.Tenant,
				Profile: req.Profile,
				Ready:   true,
				Message: msg,
			}, nil
		}
	}
	if file != "" {
		if err := s.reconcileTenantFile(ctx, file, req.Wait); err != nil {
			return nil, fmt.Errorf("reconcile tenant manifest: %w", err)
		}
	}

	if status == "" {
		status = "installed"
	}
	if !req.Wait {
		ready, msg, err := s.appReadyState(ctx, req.Tenant, req.Profile)
		if err != nil {
			return nil, err
		}
		if !ready && msg == "" {
			msg = "Install requested — provisioning in progress"
		}
		return &Result{
			Status:  status,
			Tenant:  req.Tenant,
			Profile: req.Profile,
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
		Ready:   true,
		Message: "Installed and ready",
	}, nil
}

// Uninstall removes the profile from git, reconciles, waits for removal, and optionally purges.
func (s *Service) Uninstall(ctx context.Context, req UninstallRequest) (*Result, error) {
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

	status, file, changed, err := s.git.Uninstall(req.Tenant, req.Profile, req.Actor)
	if err != nil {
		return nil, err
	}
	if status == "not_installed" && !s.appClaimExists(ctx, req.Tenant, req.Profile) {
		return &Result{Status: status, Tenant: req.Tenant, Profile: req.Profile}, nil
	}
	if changed && file != "" {
		if err := s.reconcileTenantFile(ctx, file, false); err != nil {
			return nil, fmt.Errorf("reconcile tenant manifest: %w", err)
		}
	}

	if err := s.waitForAppUninstalled(ctx, req.Tenant, req.Profile, s.opts.WaitTimeout); err != nil {
		return nil, err
	}

	var warnings []string
	if req.Purge {
		warnings = s.purge(ctx, tenant, profileCR, req.Profile)
	}

	return &Result{
		Status:   status,
		Tenant:   req.Tenant,
		Profile:  req.Profile,
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

func (s *Service) appClaimExists(ctx context.Context, tenant, profile string) bool {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	err := s.client.Get(ctx, client.ObjectKey{Name: profile, Namespace: tenantNamespace(tenant)}, obj)
	return err == nil
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
