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
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/customization"
	"github.com/gentian-org/gentian-os/internal/usage"
)

const platformAppAnnotation = "gentianos.io/platform-app"

var appClaimGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "App",
}

// xAppGVK is the composite behind an App claim. Teardown is only finished once
// this is gone too — see waitForAppUninstalled.
var xAppGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "XApp",
}

// Service implements tenant app install/uninstall/purge via GitOps.
type Service struct {
	client    client.Client
	clientset kubernetes.Interface
	opts      Options
	git       *GitOps
	// actualSource reads live consumption for the resources API. Nil when the
	// cluster has no metrics source, which is a supported configuration: the
	// ceiling and the committed usage under it come from the API server, and
	// those are the figures a plan is chosen and billed on.
	actualSource usage.ActualSource
	// appLocks serializes lifecycle operations per (tenant, profile) — see lockApp.
	appLocks sync.Map
}

// lockApp blocks until no other lifecycle operation is running for this app,
// and returns the release function.
//
// Install, Uninstall and SetAddons each read the tenant manifest, rewrite it,
// and then wait on the cluster to catch up; purge then deletes state on the
// assumption that nothing is putting it back. Run two of them against the same
// app at once and they interleave badly: a reinstall issued while a purge is
// still deleting gets its freshly provisioned volumes and secrets removed by
// the tail of the teardown, and the result looks like an install that half
// worked. Serializing per app costs nothing when they are for different apps,
// which is the normal case.
//
// In-process is sufficient: the operator runs a single replica, and with
// leader election on only one manager is active.
func (s *Service) lockApp(tenant, profile string) func() {
	v, _ := s.appLocks.LoadOrStore(tenant+"/"+profile, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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
	svc := &Service{
		client:    c,
		clientset: cs,
		opts:      opts,
		git:       NewGitOps(opts.DeploymentsPath, opts.DeploymentsRepo, opts.DeploymentsCluster),
	}
	// Constructed rather than probed: metrics.k8s.io may be absent, and
	// discovering that at start-up would make the operator's readiness depend
	// on an optional add-on. A source that cannot answer reports so per call,
	// where the caller can be told which series is missing and why.
	if opts.MetricsEnabled {
		src, err := usage.NewMetricsAPISource(cfg)
		if err != nil {
			return nil, fmt.Errorf("build metrics source: %w", err)
		}
		svc.actualSource = src
	}
	return svc, nil
}

// Install commits the profile to gentian-deployments, reconciles, and waits until Ready.
func (s *Service) Install(ctx context.Context, req InstallRequest) (*Result, error) {
	defer s.lockApp(req.Tenant, req.Profile)()

	if err := s.validateProfile(ctx, req.Profile); err != nil {
		return nil, err
	}

	status, file, _, err := s.git.Install(req.Tenant, req.Profile, req.Actor)
	if err != nil {
		return nil, err
	}
	if status == "already_installed" {
		if req.Provision {
			if err := s.provisionAppGroupUsers(ctx, req.Tenant, req.Profile); err != nil {
				return nil, fmt.Errorf("failed to provision users: %w", err)
			}
			// Readiness is the app's, not this call's. Granting group access
			// says nothing about whether the workload is running, and reporting
			// Ready unconditionally here told the user "X is ready" while the
			// app list — correctly — still showed it installing.
			ready, msg, err := s.appReadyState(ctx, req.Tenant, req.Profile)
			if err != nil {
				return nil, err
			}
			if !ready && msg == "" {
				msg = "Access granted — the application is still starting"
			}
			if ready {
				msg = "All existing users successfully added to the application access group."
			}
			return &Result{
				Status:  "provisioned",
				Tenant:  req.Tenant,
				Profile: req.Profile,
				Ready:   ready,
				Message: msg,
			}, nil
		}
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
		if req.Provision {
			if err := s.provisionAppGroupUsers(ctx, req.Tenant, req.Profile); err != nil {
				return nil, fmt.Errorf("failed to provision users: %w", err)
			}
		}
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
	if req.Provision {
		if err := s.provisionAppGroupUsers(ctx, req.Tenant, req.Profile); err != nil {
			return nil, fmt.Errorf("failed to provision users: %w", err)
		}
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
	defer s.lockApp(req.Tenant, req.Profile)()

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
		// A purge that leaves state behind must not report success. The app is gone
		// from the tenant either way — that part already happened — but the caller
		// needs to know the teardown is incomplete, because the residue is what
		// blocks the next install rather than anything visible at the time.
		if len(warnings) > 0 {
			return nil, fmt.Errorf(
				"purge of %s did not complete: %s", req.Profile, strings.Join(warnings, "; "))
		}
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

func (s *Service) loadKeycloakAdmin(ctx context.Context) (string, string, string, error) {
	secret := &corev1.Secret{}
	err := s.client.Get(ctx, types.NamespacedName{Name: "keycloak-admin", Namespace: "platform-kernel"}, secret)
	if err != nil {
		return "", "", "", err
	}
	u := string(secret.Data["url"])
	user := string(secret.Data["username"])
	pass := string(secret.Data["password"])
	if u == "" || user == "" || pass == "" {
		return "", "", "", fmt.Errorf("keycloak-admin secret is incomplete")
	}
	return u, user, pass, nil
}

func (s *Service) provisionAppGroupUsers(ctx context.Context, tenantName, profileName string) error {
	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gentianos.io",
		Version: "v1alpha1",
		Kind:    "AppProfile",
	})
	err := s.client.Get(ctx, client.ObjectKey{Name: profileName}, profile)
	if err != nil {
		return fmt.Errorf("failed to get AppProfile %s: %w", profileName, err)
	}

	var attrs map[string][]string
	annotations := profile.GetAnnotations()
	if annotations != nil {
		if val, ok := annotations["gentianos.io/keycloak-group-attributes"]; ok {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(val), &parsed); err == nil {
				attrs = make(map[string][]string)
				for k, v := range parsed {
					if list, ok := v.([]any); ok {
						var strList []string
						for _, item := range list {
							strList = append(strList, fmt.Sprint(item))
						}
						attrs[k] = strList
					} else if valStr, ok := v.(string); ok {
						attrs[k] = []string{valStr}
					} else {
						attrs[k] = []string{fmt.Sprint(v)}
					}
				}
			}
		}
	}

	kcURL, kcUser, kcPass, err := s.loadKeycloakAdmin(ctx)
	if err != nil {
		return fmt.Errorf("load keycloak admin credentials: %w", err)
	}

	kc := authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass)
	fullGroupName := fmt.Sprintf("gentian:tenant:%s:app:%s", tenantName, profileName)

	groupID, err := kc.EnsureGroup(ctx, tenantName, fullGroupName, attrs)
	if err != nil {
		return fmt.Errorf("ensure keycloak group %s: %w", fullGroupName, err)
	}
	if groupID == "" {
		return fmt.Errorf("group %s ID not found", fullGroupName)
	}

	users, err := kc.ListRealmUsers(ctx, tenantName)
	if err != nil {
		return fmt.Errorf("list keycloak users in realm %s: %w", tenantName, err)
	}

	for _, u := range users {
		if err := kc.AddUserToGroup(ctx, tenantName, u.ID, groupID); err != nil {
			return fmt.Errorf("failed to add user %s to group %s: %w", u.Username, fullGroupName, err)
		}
	}

	return nil
}

// SetAddons replaces the tenant's addon selection for one installed app.
//
// Selections are validated against the catalogue before anything is written, so a
// bad request fails with a message instead of committing a tenant file that the
// operator will later reject. Entitlement is checked here too: a commercial addon
// needs a grant, and technical compatibility is never the gate.
func (s *Service) SetAddons(ctx context.Context, req SetAddonsRequest) (*Result, error) {
	defer s.lockApp(req.Tenant, req.Profile)()

	base := &gentianov1alpha1.AppProfile{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: req.Profile}, base); err != nil {
		return nil, fmt.Errorf("get appprofile %q: %w", req.Profile, err)
	}

	profiles := &gentianov1alpha1.AppProfileList{}
	if err := s.client.List(ctx, profiles); err != nil {
		return nil, fmt.Errorf("list appprofiles: %w", err)
	}
	index := make(map[string]*gentianov1alpha1.AppProfile, len(profiles.Items))
	for i := range profiles.Items {
		index[profiles.Items[i].Name] = &profiles.Items[i]
	}

	resolved, errs := customization.ResolveAddons(base, req.Addons, index)
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return nil, fmt.Errorf("invalid addon selection: %s", strings.Join(msgs, "; "))
	}
	if _, blocked := customization.EntitledAddons(resolved, nil); len(blocked) > 0 {
		names := make([]string, 0, len(blocked))
		for _, b := range blocked {
			names = append(names, b.Profile)
		}
		return nil, fmt.Errorf(
			"these addons require a commercial subscription: %s", strings.Join(names, ", "))
	}

	status, file, changed, err := s.git.SetAddons(req.Tenant, req.Profile, req.Addons, req.Actor)
	if err != nil {
		return nil, err
	}
	if changed && file != "" {
		if err := s.reconcileTenantFile(ctx, file, false); err != nil {
			return nil, fmt.Errorf("reconcile tenant manifest: %w", err)
		}
	}

	// Installing an addon activates it; it does not decide who may see it. Access
	// comes from membership of the addon's own group, which carries the grant
	// attributes declared on its profile. Provision is the same shortcut it is for
	// an app: put every existing tenant user in that group now, rather than leaving
	// the admin to assign them.
	var warnings []string
	if req.Provision {
		for _, addon := range resolved {
			if err := s.provisionAppGroupUsers(ctx, req.Tenant, addon.Profile); err != nil {
				warnings = append(warnings,
					fmt.Sprintf("provision access for %s: %v", addon.Profile, err))
			}
		}
	}
	return &Result{Status: status, Tenant: req.Tenant, Profile: req.Profile, Warnings: warnings}, nil
}
