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
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// What the cluster has made of a tenant's apps, and the two things that can
// be done to one of them that are not a change of desired state.
//
// Git says what a tenant has installed and the director answers that. What
// became of it -- still coming up, running, broken -- only the cluster knows,
// and so do the two acts here: purging the data an uninstalled app left
// behind, and granting an app to everybody who is already a member. Neither
// is something a tenant "is", so neither belongs in its file; both are things
// done once, by a person, and recorded by who did them.

// App phases. Three, because those are the three things a person can do
// about it: wait, use it, or look at it.
const (
	AppPhaseInstalling = "installing"
	AppPhaseReady      = "ready"
	AppPhaseFailing    = "failing"
)

const (
	deploymentRoleAnnotation = "gentianos.io/deployment-role"
	deploymentRoleAddon      = "addon"
)

// AppState is one installed app as the cluster has it.
type AppState struct {
	// Profile is the catalogue entry; Name is the Component made from it.
	Profile string `json:"profile"`
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Phase   string `json:"phase"`
	// Message is why it is in that phase, in the reconciler's own words.
	Message string `json:"message,omitempty"`
	// Failure is set when the workload itself is broken rather than slow:
	// an image that cannot be pulled, a container that keeps exiting, a pod
	// nothing can schedule. Kubernetes retries those for ever, so without
	// this an app that will never start reads as one that is still starting.
	Failure string `json:"failure,omitempty"`
	// PendingPrivileges are requests of the profile nobody has granted yet.
	// While any is listed the install waits on a person, not on the cluster.
	PendingPrivileges []string       `json:"pendingPrivileges,omitempty"`
	Conditions        []AppCondition `json:"conditions,omitempty"`
	// Reserved is what the app's pods request: what it costs against the
	// tenant's ceiling, not what it happens to be using. Absent for an app
	// with no pods.
	Reserved *AppReservation `json:"reserved,omitempty"`
}

// AppReservation is the sum of an app's container requests.
type AppReservation struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// AppCondition is one condition of the Component, as reported.
type AppCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// waitingReasons are the Ready=False reasons that resolve without anybody
// changing the install: the chart is deploying, the base is not up yet, the
// zone is being made, an approval is outstanding. Every other reason names
// something wrong with the install itself, and waiting does not fix those.
var waitingReasons = map[string]bool{
	"Installing":        true,
	"AddonBaseNotReady": true,
	"ZoneNotReady":      true,
	"PrivilegesPending": true,
}

// AppStates reports every app of a tenant as the cluster has it.
//
// The platform's own components are left out -- the desktop, the console --
// because they are not something a tenant administrator installed and the
// store offers nothing to do about them. So are add-ons: an add-on is
// switched on inside its base, and its state is the base's.
func (s *Service) AppStates(ctx context.Context, tenantName string) ([]AppState, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return nil, err
	}
	ns := layout.Tenant(tenantName)
	var list gentianov1alpha1.ComponentList
	if err := s.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list components: %w", err)
	}
	out := make([]AppState, 0, len(list.Items))
	for i := range list.Items {
		comp := &list.Items[i]
		profile := comp.Spec.ProfileRef.Name
		if profile == "" {
			continue
		}
		cp := &gentianov1alpha1.ComponentProfile{}
		if err := s.client.Get(ctx, client.ObjectKey{Name: profile}, cp); err == nil {
			if cp.Annotations[platformAppAnnotation] == "true" ||
				cp.Annotations[deploymentRoleAnnotation] == deploymentRoleAddon {
				continue
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get componentprofile %q: %w", profile, err)
		}
		state := componentState(comp)
		pods, err := s.componentPods(ctx, comp)
		if err != nil {
			return nil, err
		}
		state.Reserved = reservation(pods)
		if !state.Ready {
			failure := workloadFailure(pods)
			if failure != "" {
				state.Phase, state.Failure = AppPhaseFailing, failure
				if state.Message == "" {
					state.Message = failure
				}
			}
		}
		out = append(out, state)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Profile < out[b].Profile })
	return out, nil
}

// componentState reads a Component's own account of itself.
func componentState(comp *gentianov1alpha1.Component) AppState {
	state := AppState{
		Profile:           comp.Spec.ProfileRef.Name,
		Name:              comp.Name,
		Phase:             AppPhaseInstalling,
		Message:           "Provisioning in progress",
		PendingPrivileges: comp.Status.PendingPrivileges,
	}
	for _, c := range comp.Status.Conditions {
		state.Conditions = append(state.Conditions, AppCondition{
			Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	ready := meta.FindStatusCondition(comp.Status.Conditions, "Ready")
	if ready == nil {
		return state
	}
	state.Message = ready.Message
	switch {
	case string(ready.Status) == "True":
		state.Ready, state.Phase = true, AppPhaseReady
		if state.Message == "" {
			state.Message = "Installed and ready"
		}
	case waitingReasons[ready.Reason]:
		state.Phase = AppPhaseInstalling
	default:
		state.Phase = AppPhaseFailing
		state.Failure = ready.Reason
	}
	return state
}

// failingWaits are the container states Kubernetes retries without end.
var failingWaits = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
}

// componentPods are the pods of a component's release.
//
// Found by the release the component's chart was installed as, which is the
// label Helm puts on what it renders. A component with no pods -- an external
// service, a chart still rendering -- has nothing to find.
func (s *Service) componentPods(ctx context.Context, comp *gentianov1alpha1.Component) ([]corev1.Pod, error) {
	var pods corev1.PodList
	err := s.client.List(ctx, &pods,
		client.InNamespace(comp.Namespace),
		client.MatchingLabels{"app.kubernetes.io/instance": comp.Namespace + "-" + comp.Name})
	if err != nil {
		return nil, fmt.Errorf("list pods of %s: %w", comp.Name, err)
	}
	return pods.Items, nil
}

// reservation sums what the pods request. A finished pod holds nothing, so
// a completed Job is not counted against the app that ran it.
func reservation(pods []corev1.Pod) *AppReservation {
	var out AppReservation
	counted := false
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			out.CPUMilli += c.Resources.Requests.Cpu().MilliValue()
			out.MemoryBytes += c.Resources.Requests.Memory().Value()
			counted = true
		}
	}
	if !counted {
		return nil
	}
	return &out
}

// workloadFailure says what is wrong with a component's pods, or nothing.
func workloadFailure(pods []corev1.Pod) string {
	for i := range pods {
		pod := &pods[i]
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
				c.Reason == corev1.PodReasonUnschedulable {
				return "no node can run " + pod.Name + ": " + c.Message
			}
		}
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...),
			pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && failingWaits[w.Reason] {
				msg := w.Reason
				if w.Message != "" {
					msg += ": " + w.Message
				}
				return cs.Name + " in " + pod.Name + " — " + msg
			}
		}
	}
	return ""
}

// ErrStillInstalled is a purge asked of an app the tenant still has.
var ErrStillInstalled = errors.New("the app is still installed")

// ErrStillRemoving is a purge asked while the cluster is still taking the app
// down. Not a failure: the same request succeeds a little later.
var ErrStillRemoving = errors.New("the app is still being removed")

// purgeWait is how long a purge waits for the teardown to finish before
// saying "not yet". Short, because the director's request to this API has a
// deadline of its own and an answer that arrives after it is no answer; the
// caller asks again, which is what the store's status poll does anyway.
const purgeWait = 20 * time.Second

// PurgeApp deletes what an uninstalled app left behind: its databases, its
// object storage, its secrets and the kernel's provisioning records for it.
//
// Only ever after an uninstall. Uninstalling is a change to what the tenant
// is, made in git by the director; this is the irreversible half, and it is
// kept a separate act so that taking an app away never takes its data with it
// by accident. It waits for the component to be gone, because a purge that
// races the teardown deletes a database a pod is still writing to.
func (s *Service) PurgeApp(ctx context.Context, tenantName, profile, actor string) (*Result, error) {
	defer s.lockApp(tenantName, profile)()

	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	for _, a := range tenant.Spec.Apps {
		if a.Profile == profile {
			return nil, fmt.Errorf("%w: uninstall %s before purging it", ErrStillInstalled, profile)
		}
	}
	if err := s.waitForComponentGone(ctx, tenantName, profile, purgeWait); err != nil {
		return nil, err
	}

	cp := &gentianov1alpha1.ComponentProfile{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: profile}, cp); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get componentprofile %q: %w", profile, err)
		}
		// A profile the cluster no longer has: purge what every app has.
		cp = nil
	}
	if warnings := s.purge(ctx, tenant, cp, profile); len(warnings) > 0 {
		// A purge that leaves state behind must not report success: the
		// residue is what blocks the next install.
		return nil, fmt.Errorf("purge of %s did not complete: %s", profile, strings.Join(warnings, "; "))
	}
	return &Result{Status: "purged", Tenant: tenantName, Profile: profile, Purged: true,
		Message: "purged by " + actor}, nil
}

// waitForComponentGone blocks until no Component of the profile is left in
// the tenant's namespace.
func (s *Service) waitForComponentGone(ctx context.Context, tenantName, profile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ns := layout.Tenant(tenantName)
	for {
		var list gentianov1alpha1.ComponentList
		if err := s.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return fmt.Errorf("list components: %w", err)
		}
		left := false
		for i := range list.Items {
			if list.Items[i].Spec.ProfileRef.Name == profile {
				left = true
				break
			}
		}
		if !left {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s in %s; purge it once it is gone", ErrStillRemoving, profile, tenantName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// ProvisionApp grants an installed app to everybody who is already a member
// of the tenant, and marks it as granted by default to whoever joins later.
//
// Installing makes an app exist; who may use it is decided separately. This
// is the shortcut for the common answer, "everyone".
func (s *Service) ProvisionApp(ctx context.Context, tenantName, profile string) (*Result, error) {
	defer s.lockApp(tenantName, profile)()

	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	// An app of the tenant's, or an add-on switched on inside one: an add-on
	// has a group of its own, and who may use it is decided separately from
	// who may use its base.
	installed := false
	for _, a := range tenant.Spec.Apps {
		if a.Profile == profile {
			installed = true
			break
		}
		for _, addon := range a.Addons {
			if addon == profile {
				installed = true
				break
			}
		}
	}
	if !installed {
		return nil, fmt.Errorf("%s is not installed in %s", profile, tenantName)
	}
	if err := s.provisionAppGroupUsers(ctx, tenantName, profile); err != nil {
		return nil, err
	}
	return &Result{Status: "provisioned", Tenant: tenantName, Profile: profile}, nil
}

func (h *HTTPServer) registerAppRoutes(mux router) {
	mux.HandleFunc("GET /v1/tenants/{tenant}/apps/status", h.handleAppStates)
	// Actions, not writes. Desired state is the director's, in git; these are
	// things done once.
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/purge-app", h.handlePurgeApp)
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/provision-app", h.handleProvisionApp)
}

func (h *HTTPServer) handleAppStates(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	apps, err := h.Service.AppStates(r.Context(), tenant)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": tenant, "apps": apps})
}

func profileOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Profile string `json:"profile"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return "", false
	}
	if body.Profile == "" {
		writeErr(w, http.StatusBadRequest, errors.New("profile is required"))
		return "", false
	}
	return body.Profile, true
}

func (h *HTTPServer) handlePurgeApp(w http.ResponseWriter, r *http.Request) {
	profile, ok := profileOf(w, r)
	if !ok {
		return
	}
	res, err := h.Service.PurgeApp(r.Context(), r.PathValue("tenant"), profile, actorOf(r))
	switch {
	case errors.Is(err, ErrStillInstalled), errors.Is(err, ErrStillRemoving):
		writeErr(w, http.StatusConflict, err)
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (h *HTTPServer) handleProvisionApp(w http.ResponseWriter, r *http.Request) {
	profile, ok := profileOf(w, r)
	if !ok {
		return
	}
	res, err := h.Service.ProvisionApp(r.Context(), r.PathValue("tenant"), profile)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
