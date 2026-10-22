/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/credentialprobe"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/security"
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
	PendingPrivileges []string `json:"pendingPrivileges,omitempty"`
	// PendingPrivilegeApprovals is the same list, each with who answers it,
	// so that a console offers an approval only to whom the director would
	// allow one. Additive: a reader that knows only pendingPrivileges is
	// told the same requests.
	PendingPrivilegeApprovals []PendingPrivilege `json:"pendingPrivilegeApprovals,omitempty"`
	Conditions                []AppCondition     `json:"conditions,omitempty"`
	// AwaitingCredentials are the credentials the app's profile asks a
	// person for (spec.secrets.supplied) that are not set yet, while the
	// app is not up: it waits for them. A bundle carries none, so an
	// imported app waits for them until they are set.
	AwaitingCredentials []credentialprobe.Awaited `json:"awaitingCredentials,omitempty"`
	// Reserved is what the app's pods request: what it costs against the
	// tenant's ceiling, not what it happens to be using. Absent for an app
	// with no pods.
	Reserved *AppReservation `json:"reserved,omitempty"`
	// Addons are the add-ons the app activates (spec.addons) that the
	// cluster holds a Component for. Each is a Component of its own, named
	// for its profile, and its privileges are asked, approved and declined
	// under that name, not the app's.
	Addons []AddonState `json:"addons,omitempty"`
}

// AddonState is one add-on of an app, as its own Component has it.
type AddonState struct {
	Profile                   string             `json:"profile"`
	Name                      string             `json:"name"`
	Ready                     bool               `json:"ready"`
	Phase                     string             `json:"phase"`
	Message                   string             `json:"message,omitempty"`
	PendingPrivileges         []string           `json:"pendingPrivileges,omitempty"`
	PendingPrivilegeApprovals []PendingPrivilege `json:"pendingPrivilegeApprovals,omitempty"`
}

// PendingPrivilege is one request nobody has granted yet, and who may.
type PendingPrivilege struct {
	// Privilege is <kind>/<name>, as in PendingPrivileges.
	Privilege string `json:"privilege"`
	// ApprovedBy is who answers it: "perimeter-approver" for the tenant's
	// perimeter approver (tenant#can_approve_privilege), "security-officer" for the cluster's
	// security officer (cluster#can_approve). security.PrivilegeApprover
	// decides it from the kind, as the director does on the approval.
	ApprovedBy string `json:"approvedBy"`
	// Reason is what the profile said it needs the privilege for, which is
	// what an approver reads before deciding. Empty when the profile gave
	// none, or is not on the cluster.
	Reason string `json:"reason,omitempty"`
	// RequestedBy, RequestedAt and Note are the request a tenant
	// administrator made of the security officer (privilege_requests.go),
	// absent while nobody asked. A perimeter-approved privilege has none.
	RequestedBy string `json:"requestedBy,omitempty"`
	RequestedAt string `json:"requestedAt,omitempty"`
	Note        string `json:"note,omitempty"`
	// ExpiredAt is when a grant for this privilege stopped applying, for a
	// request that was approved once and waits again.
	ExpiredAt string `json:"expiredAt,omitempty"`
	// DeclinedBy, DeclinedAt and DeclineReason are an approver's no
	// (privilege_requests.go): who, when and why. The privilege is still
	// pending -- a decline installs nothing and the app keeps waiting -- but
	// whoever asked is told. Cleared by a new request, and by an approval
	// made after it.
	DeclinedBy    string `json:"declinedBy,omitempty"`
	DeclinedAt    string `json:"declinedAt,omitempty"`
	DeclineReason string `json:"declineReason,omitempty"`
}

// pendingApprovals pairs each pending request with who answers it, what the
// profile said it is for, and what has been asked of the approver so far.
// profile is nil for one the cluster no longer has.
func pendingApprovals(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, now time.Time) []PendingPrivilege {
	pending := comp.Status.PendingPrivileges
	if len(pending) == 0 {
		return nil
	}
	requests := requestsOf(comp)
	out := make([]PendingPrivilege, 0, len(pending))
	for _, ref := range pending {
		kind, _ := security.SplitPrivilegeRef(ref)
		p := PendingPrivilege{
			Privilege:  ref,
			ApprovedBy: security.PrivilegeApprover(kind),
			Reason:     security.PrivilegeReason(profile, ref),
			ExpiredAt:  expiredGrantAt(comp, ref, now),
		}
		if req, ok := requests[ref]; ok {
			if p.ApprovedBy == security.PrivilegeApproverSecurityOfficer {
				p.RequestedBy, p.RequestedAt, p.Note = req.RequestedBy, req.RequestedAt, req.Note
			}
			p.DeclinedBy, p.DeclinedAt, p.DeclineReason = declineOf(comp, req, ref)
		}
		out = append(out, p)
	}
	return out
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

// componentOriginLabel says who put a component in the tenant, as the
// operator labels it (internal/controller, componentOriginLabel): "default"
// for one the platform places in every tenant, "install" for one the
// tenant installed.
const (
	componentOriginLabel   = "gentianos.io/component-origin"
	componentOriginDefault = "default"
)

// AppStates reports every app of a tenant as the cluster has it.
//
// The platform's own components are left out -- the desktop, the consoles,
// the App Store app -- because they are not something a tenant administrator
// installed and the store offers nothing to do about them. They are known by
// the operator's label on the component; the profile's platform-app
// annotation still counts, but a profile need not carry it, and one that did
// not was listed as an app the tenant had uninstalled. So are add-ons left
// out as apps: an add-on is switched on inside its base, and is listed under
// it, in the base's addons.
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
	bases := make([]*gentianov1alpha1.Component, 0, len(list.Items))
	addons := map[string]AddonState{}
	for i := range list.Items {
		comp := &list.Items[i]
		profile := comp.Spec.ProfileRef.Name
		if profile == "" || comp.Labels[componentOriginLabel] == componentOriginDefault {
			continue
		}
		cp := &gentianov1alpha1.ComponentProfile{}
		found := false
		if err := s.client.Get(ctx, client.ObjectKey{Name: profile}, cp); err == nil {
			found = true
			if cp.Annotations[platformAppAnnotation] == "true" {
				continue
			}
			if cp.Annotations[deploymentRoleAnnotation] == deploymentRoleAddon {
				addons[comp.Name] = addonState(comp, cp)
				continue
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get componentprofile %q: %w", profile, err)
		}
		state := componentState(comp)
		if found {
			state.PendingPrivilegeApprovals = pendingApprovals(comp, cp, time.Now())
		}
		pods, err := s.componentPods(ctx, comp)
		if err != nil {
			return nil, err
		}
		state.Reserved = reservation(pods)
		if !state.Ready {
			if found {
				state.AwaitingCredentials = credentialprobe.AwaitedSupplied(ctx, s.client, tenantName, comp.Name, cp)
			}
			failure := workloadFailure(pods)
			if failure != "" {
				state.Phase, state.Failure = AppPhaseFailing, failure
				if state.Message == "" {
					state.Message = failure
				}
			}
		}
		out = append(out, state)
		bases = append(bases, comp)
	}
	for i, comp := range bases {
		for _, name := range comp.Spec.Addons {
			if addon, ok := addons[name]; ok {
				out[i].Addons = append(out[i].Addons, addon)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Profile < out[b].Profile })
	return out, nil
}

// addonState is an add-on Component's account of itself, with its pending
// privileges and who answers each.
func addonState(comp *gentianov1alpha1.Component, cp *gentianov1alpha1.ComponentProfile) AddonState {
	state := componentState(comp)
	return AddonState{
		Profile: state.Profile, Name: state.Name, Ready: state.Ready, Phase: state.Phase, Message: state.Message,
		PendingPrivileges:         state.PendingPrivileges,
		PendingPrivilegeApprovals: pendingApprovals(comp, cp, time.Now()),
	}
}

// componentState reads a Component's own account of itself.
func componentState(comp *gentianov1alpha1.Component) AppState {
	state := AppState{
		Profile:                   comp.Spec.ProfileRef.Name,
		Name:                      comp.Name,
		Phase:                     AppPhaseInstalling,
		Message:                   "Provisioning in progress",
		PendingPrivileges:         comp.Status.PendingPrivileges,
		PendingPrivilegeApprovals: pendingApprovals(comp, nil, time.Now()),
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

// ErrBusy is a purge asked while another operation on the same app is
// running. Not queued behind it: a request that waited its turn could outlast
// its caller, and a purge is not something to start after the person who
// asked has been told it did not happen.
var ErrBusy = errors.New("another operation on this app is running")

// ErrProfileMissing is a purge asked of an app whose ComponentProfile is not
// on the cluster. Nothing is destroyed.
var ErrProfileMissing = errors.New("the app's profile is missing")

// ErrNotAnApp is a purge asked of a name that is not an app's.
var ErrNotAnApp = errors.New("not an app")

// purgeWait is how long a purge waits for the teardown to finish before
// saying "not yet". Short: the person asking is better served by "still being
// removed, ask again" than by a request that sits for minutes before it has
// destroyed anything.
var purgeWait = 20 * time.Second

// PurgeApp destroys what an uninstalled app left behind: its database, its
// object storage and cache user, its files, its stored credentials, its
// access group and the kernel's provisioning records for it.
//
// Only ever after an uninstall. Uninstalling is a change to what the tenant
// is, made in git by the director, and it keeps all of the above; this is the
// irreversible half, and it is kept a separate act so that taking an app away
// never takes its data with it by accident. It is refused while the tenant
// still has the app, and it waits for the app to be gone from the cluster,
// because a purge that races the teardown deletes a database a pod is still
// writing to.
//
// One request, answered when it is over -- see purge.go. A purge that did not
// complete returns a *PurgeError naming the step that failed and what was
// already destroyed; the remedy is to ask again.
func (s *Service) PurgeApp(ctx context.Context, tenantName, profile, actor string) (*Result, error) {
	unlock, ok := s.tryLockApp(tenantName, profile)
	if !ok {
		return nil, fmt.Errorf("%w: %s in %s; ask again when it has finished", ErrBusy, profile, tenantName)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, purgeBudget)
	defer cancel()

	// A name the platform keeps its own stores under is not an app, and
	// "not installed" would otherwise be true of it: the desktop's database
	// is recorded like an app's.
	if backup.IsPlatformStore(profile) {
		return nil, fmt.Errorf("%w: %s names stores the platform keeps for the tenant itself", ErrNotAnApp, profile)
	}
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	// An app of the tenant's, or an add-on switched on inside one. An add-on
	// has an access group of its own, and purging one that is still switched
	// on would take that group from the people using it.
	for _, a := range tenant.Spec.Apps {
		if a.Profile == profile {
			return nil, fmt.Errorf("%w: uninstall %s before purging it", ErrStillInstalled, profile)
		}
		for _, addon := range a.Addons {
			if addon == profile {
				return nil, fmt.Errorf("%w: %s is switched on in %s; switch it off before purging it",
					ErrStillInstalled, profile, a.Profile)
			}
		}
	}

	// Which stores an app has is declared by its profile and by nothing
	// else. Without it a purge could only guess -- it used to assume a
	// PostgreSQL database and look at nothing more -- and a purge that
	// guesses either destroys what it should not or reports as purged an
	// app whose bucket is still there. So it is refused, with everything in
	// place.
	cp := &gentianov1alpha1.ComponentProfile{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: profile}, cp); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get componentprofile %q: %w", profile, err)
		}
		return nil, fmt.Errorf("%w: the ComponentProfile %s is not on this cluster, and what the app owns in %s "+
			"cannot be determined without it. Nothing was destroyed. A profile is placed on the cluster when the app "+
			"is installed from a catalogue source that serves it: make %s available again that way -- installing it "+
			"in a tenant puts its profile back -- then uninstall it here if it was installed here, and purge",
			ErrProfileMissing, profile, tenantName, profile)
	}
	if err := s.waitForAppGone(ctx, tenant, profile, cp, purgeWait); err != nil {
		return nil, err
	}
	// Everything that can be known beforehand about whether the purge can
	// finish, before the first thing is destroyed.
	recorded, err := s.provisioned(ctx, tenantName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v. Nothing was destroyed; ask again once that is put right", ErrCannotPurgeNow, err)
	}
	if err := s.purgePreflight(ctx, tenant, cp, recorded[profile]); err != nil {
		return nil, err
	}

	log.FromContext(ctx).WithName("purge").Info("purging an app", "tenant", tenantName, "app", profile, "actor", actor)
	destroyed, err := s.purge(ctx, tenant, cp, profile, recorded[profile])
	if err != nil {
		return nil, err
	}
	return &Result{Status: "purged", Tenant: tenantName, Profile: profile, Purged: true,
		Complete: ptr(true), Destroyed: destroyed, Message: "purged by " + actor}, nil
}

// appRemnants names what is still on the cluster of an app's workloads: its
// Component, and the Helm releases it was installed as.
//
// The Component alone is not the answer. It goes as soon as its App claim is
// handed to the garbage collector, and the release the claim composed is
// uninstalled after that; for a while the app has no Component and running
// pods. Helm's own record of a release -- a Secret in the release's namespace
// -- is there until `helm uninstall` has finished, and the release names are
// the app's own (backup.AppRelease and its neighbours), so that record is
// what says whether the workloads are gone.
func (s *Service) appRemnants(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile string, cp *gentianov1alpha1.ComponentProfile) ([]string, error) {
	ns := tenantNamespace(tenant)
	var left []string
	var list gentianov1alpha1.ComponentList
	if err := s.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list components: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.ProfileRef.Name == profile {
			left = append(left, "component "+list.Items[i].Name)
		}
	}
	releases, err := s.helmReleases(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, release := range releases {
		if ownsRelease(tenant, profile, cp, release) {
			left = append(left, "Helm release "+release)
		}
	}
	return left, nil
}

// helmReleases names the Helm releases recorded in a namespace.
func (s *Service) helmReleases(ctx context.Context, ns string) ([]string, error) {
	records, err := s.clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list the Helm releases in %s: %w", ns, err)
	}
	seen := map[string]struct{}{}
	var out []string
	for i := range records.Items {
		rec := &records.Items[i]
		name := rec.Labels["name"]
		if rec.Type != helmReleaseSecretType || name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// helmReleaseSecretType is the type of the Secret Helm keeps one revision of
// a release in.
const helmReleaseSecretType = corev1.SecretType("helm.sh/release.v1")

// waitForAppGone blocks until nothing of the app's workloads is left in the
// tenant's namespace.
func (s *Service) waitForAppGone(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile string, cp *gentianov1alpha1.ComponentProfile, timeout time.Duration) error {
	tenantName := tenant.Name
	deadline := time.Now().Add(timeout)
	for {
		left, err := s.appRemnants(ctx, tenant, profile, cp)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s in %s (%s); nothing was destroyed — purge it once it is gone",
				ErrStillRemoving, profile, tenantName, strings.Join(left, ", "))
		}
		if err := wait(ctx); err != nil {
			return err
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
	if err := s.provisionAppGroupUsers(ctx, tenant, profile); err != nil {
		return nil, err
	}
	return &Result{Status: "provisioned", Tenant: tenantName, Profile: profile}, nil
}

func (h *HTTPServer) registerAppRoutes(mux router) {
	mux.Read("GET /v1/tenants/{tenant}/apps/status", h.handleAppStates)
	// What uninstalled apps still hold. A read: it destroys nothing and runs
	// nothing.
	mux.Read("GET /v1/tenants/{tenant}/apps/retained", h.handleRetainedApps)
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
	var incomplete *PurgeError
	switch {
	case errors.Is(err, ErrStillInstalled), errors.Is(err, ErrStillRemoving), errors.Is(err, ErrBusy),
		errors.Is(err, ErrProfileMissing):
		// Nothing was destroyed, and the request can be made again.
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, ErrCannotPurgeNow):
		// Something the purge needs is not there. Nothing was destroyed.
		writeErr(w, http.StatusServiceUnavailable, err)
	case errors.As(err, &incomplete):
		// A purge that began and did not finish. Not the caller's mistake,
		// so not a 4xx; the message says what was destroyed and to retry.
		writeErr(w, http.StatusInternalServerError, err)
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, fmt.Errorf(
			"the purge ran out of the %s it is given before it destroyed anything; retry it: %w", purgeBudget, err))
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (h *HTTPServer) handleRetainedApps(w http.ResponseWriter, r *http.Request) {
	res, err := h.Service.RetainedApps(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
