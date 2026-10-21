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
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/security"
)

// Asking the security officer.
//
// A profile's pod-security waiver or cluster role is answered at cluster
// scope, by somebody who is deliberately not an administrator of the tenant
// that installed the app (AD-5). The tenant's administrator can only ask.
// Asking is recorded here, on the Component that waits: who asked, when and
// what they added, in one annotation the operator reads back into the app's
// state and into the cluster's list of what awaits the security officer.
//
// It is an annotation and not a commit because a request decides nothing. A
// grant is declared state -- it changes what the platform gives the app, and
// git keeps who said yes -- and a request is a note on the thing that waits,
// which is why it lives with the Component's own status rather than beside
// the grants. The operator already writes the Component's metadata
// (componentOriginLabel); nothing re-renders it over this.
//
// A decline is the same kind of note. Saying no changes nothing the platform
// gives the app -- the privilege stays pending and the install keeps
// waiting, exactly as before the answer -- so it is recorded beside the
// request it answers, with who said no, when and why, and not in git. The
// director authorises it as it authorises the approval of that kind.

// privilegeRequestsAnnotation holds every request made of this Component,
// and every decline, as JSON keyed by <kind>/<name>. One annotation rather
// than one per request: a privilege's name can be long enough that
// <kind>.<name> would not fit an annotation key.
const privilegeRequestsAnnotation = "gentianos.io/privilege-requests"

// PrivilegeRequest is one request as it was made, and the decline it got, if
// any. A privilege nobody asked for can still be declined: the entry then
// carries the decline alone.
type PrivilegeRequest struct {
	// RequestedBy is the person who asked, as the director named them on the
	// action: the address of their account.
	RequestedBy string `json:"requestedBy,omitempty"`
	// RequestedAt is when, RFC 3339.
	RequestedAt string `json:"requestedAt,omitempty"`
	// Note is what they added for the approver, if anything.
	Note string `json:"note,omitempty"`
	// DeclinedBy, DeclinedAt and DeclineReason are the approver's no: who, as
	// the director named them, when (RFC 3339), and why in their own words.
	// Empty while nobody declined, and cleared by a new request.
	DeclinedBy    string `json:"declinedBy,omitempty"`
	DeclinedAt    string `json:"declinedAt,omitempty"`
	DeclineReason string `json:"declineReason,omitempty"`
}

// declined says whether this entry carries a decline.
func (r PrivilegeRequest) declined() bool { return r.DeclinedAt != "" }

// noteLimit bounds what a request carries: a note for an approver, not an
// essay in an annotation. A decline's reason is bounded the same way.
const noteLimit = 1000

// declineReasonMinimum is the least a decline's reason may be: the minimum a
// grant's reason has (the CRD's), so that saying no takes as many words as
// saying yes.
const declineReasonMinimum = 10

// ErrNotPending says the privilege named is not one the app waits for.
var ErrNotPending = errors.New("the app is not waiting for that privilege")

// ErrTenantAnswers says the privilege is the tenant administrator's own to
// approve, so there is nobody to ask.
var ErrTenantAnswers = errors.New("the tenant's administrator approves that privilege; there is nobody to ask")

// RequestOutcome is what a request came to.
type RequestOutcome struct {
	// Status is "requested" for a request made now, "unchanged" for one that
	// was already there -- asking twice records the first asking, unless the
	// first was declined, in which case asking again is a new request.
	Status    string           `json:"status"`
	Tenant    string           `json:"tenant"`
	Profile   string           `json:"profile"`
	Install   string           `json:"install"`
	Privilege string           `json:"privilege"`
	Request   PrivilegeRequest `json:"request"`
}

// requestsOf reads the Component's requests. An annotation that does not
// parse is read as none: a request is a note, and a broken note is not a
// reason to refuse the app's state.
func requestsOf(comp *gentianov1alpha1.Component) map[string]PrivilegeRequest {
	out := map[string]PrivilegeRequest{}
	raw := comp.Annotations[privilegeRequestsAnnotation]
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// grantedAfter says whether the Component carries a grant for ref approved
// after at, RFC 3339. A decline answered a request as it stood; an approval
// made since -- live or run out -- supersedes it, so the decline is no longer
// shown even when the entry outlived it on the annotation.
func grantedAfter(comp *gentianov1alpha1.Component, ref, at string) bool {
	for i := range comp.Spec.Privileges {
		g := &comp.Spec.Privileges[i]
		if g.Privilege == ref && g.ApprovedAt.UTC().Format(time.RFC3339) > at {
			return true
		}
	}
	return false
}

// declineOf is the decline the entry carries for ref, as far as it still
// stands: none when nobody declined, or when a grant came after it.
func declineOf(comp *gentianov1alpha1.Component, req PrivilegeRequest, ref string) (by, at, reason string) {
	if !req.declined() || grantedAfter(comp, ref, req.DeclinedAt) {
		return "", "", ""
	}
	return req.DeclinedBy, req.DeclinedAt, req.DeclineReason
}

// expiredGrantAt is when the Component's grant for ref stopped applying, ""
// when it carries none or a live one. A pending privilege with this set is
// one that was approved once and is waiting again.
func expiredGrantAt(comp *gentianov1alpha1.Component, ref string, now time.Time) string {
	var latest string
	for i := range comp.Spec.Privileges {
		g := &comp.Spec.Privileges[i]
		if g.Privilege != ref || g.ExpiresAt == nil || now.Before(g.ExpiresAt.Time) {
			continue
		}
		if at := g.ExpiresAt.UTC().Format(time.RFC3339); at > latest {
			latest = at
		}
	}
	return latest
}

// RequestPrivilege records that actor asked for one privilege the app waits
// for and the tenant cannot approve itself.
func (s *Service) RequestPrivilege(ctx context.Context, tenantName, profile, privilege, note, actor string) (RequestOutcome, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return RequestOutcome{}, err
	}
	kind, name := security.SplitPrivilegeRef(privilege)
	if kind == "" || name == "" {
		return RequestOutcome{}, fmt.Errorf("a privilege is <kind>/<name>: not %q", privilege)
	}
	if security.PrivilegeApprover(kind) != security.PrivilegeApproverSecurityOfficer {
		return RequestOutcome{}, ErrTenantAnswers
	}
	note = strings.TrimSpace(note)
	if len(note) > noteLimit {
		return RequestOutcome{}, fmt.Errorf("a note is at most %d characters", noteLimit)
	}
	var comp gentianov1alpha1.Component
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: layout.Tenant(tenantName), Name: profile}, &comp); err != nil {
		if apierrors.IsNotFound(err) {
			return RequestOutcome{}, fmt.Errorf("%w: the cluster holds no component %s for tenant %s yet", ErrNotPending, profile, tenantName)
		}
		return RequestOutcome{}, err
	}
	pending := map[string]bool{}
	for _, ref := range comp.Status.PendingPrivileges {
		pending[ref] = true
	}
	if !pending[privilege] {
		return RequestOutcome{}, fmt.Errorf("%w: %s waits for %s", ErrNotPending, profile, orNone(comp.Status.PendingPrivileges))
	}
	requests := requestsOf(&comp)
	out := RequestOutcome{
		Status: "unchanged", Tenant: tenantName, Profile: profile, Install: comp.Name, Privilege: privilege,
	}
	// A request that stands is not made twice. One that was declined is
	// answered, and asking again is a new request: it replaces the entry,
	// decline and all, so the approver sees it as fresh.
	if existing, ok := requests[privilege]; ok && existing.RequestedAt != "" && !existing.declined() {
		out.Request = existing
		return out, nil
	}
	// A request for something no longer pending has done its work; it goes
	// when the next one is written, so the annotation does not grow with
	// every approval the app ever waited for.
	for ref := range requests {
		if !pending[ref] {
			delete(requests, ref)
		}
	}
	out.Status = "requested"
	out.Request = PrivilegeRequest{RequestedBy: actor, RequestedAt: time.Now().UTC().Format(time.RFC3339), Note: note}
	requests[privilege] = out.Request
	if err := s.writeRequests(ctx, &comp, requests); err != nil {
		return RequestOutcome{}, fmt.Errorf("record the request on component %s: %w", comp.Name, err)
	}
	return out, nil
}

// writeRequests puts the entries on the Component, in place of what it had.
func (s *Service) writeRequests(ctx context.Context, comp *gentianov1alpha1.Component, requests map[string]PrivilegeRequest) error {
	raw, err := json.Marshal(requests)
	if err != nil {
		return err
	}
	patch := client.MergeFrom(comp.DeepCopy())
	if comp.Annotations == nil {
		comp.Annotations = map[string]string{}
	}
	comp.Annotations[privilegeRequestsAnnotation] = string(raw)
	return s.client.Patch(ctx, comp, patch)
}

// DeclineOutcome is what a decline came to.
type DeclineOutcome struct {
	// Status is "declined". Declining twice records the later decline: the
	// approver's last word is the one that stands.
	Status    string           `json:"status"`
	Tenant    string           `json:"tenant"`
	Profile   string           `json:"profile"`
	Install   string           `json:"install"`
	Privilege string           `json:"privilege"`
	Request   PrivilegeRequest `json:"request"`
}

// DeclinePrivilege records that actor said no to one privilege the app waits
// for, with their reason. Who may say it is the director's to decide, by the
// kind, as it decides who may approve; here the decline is a note on the
// Component. The privilege stays pending: nothing is installed, and the
// install keeps waiting until somebody approves or the tenant gives the app
// up. A tenant's own kind (egress) can be declined too, by whoever could
// have approved it, though nobody asks for those.
func (s *Service) DeclinePrivilege(ctx context.Context, tenantName, profile, privilege, reason, actor string) (DeclineOutcome, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return DeclineOutcome{}, err
	}
	kind, name := security.SplitPrivilegeRef(privilege)
	if kind == "" || name == "" {
		return DeclineOutcome{}, fmt.Errorf("a privilege is <kind>/<name>: not %q", privilege)
	}
	reason = strings.TrimSpace(reason)
	if len(reason) < declineReasonMinimum {
		return DeclineOutcome{}, fmt.Errorf("a decline needs a reason of at least %d characters, in your own words", declineReasonMinimum)
	}
	if len(reason) > noteLimit {
		return DeclineOutcome{}, fmt.Errorf("a reason is at most %d characters", noteLimit)
	}
	var comp gentianov1alpha1.Component
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: layout.Tenant(tenantName), Name: profile}, &comp); err != nil {
		if apierrors.IsNotFound(err) {
			return DeclineOutcome{}, fmt.Errorf("%w: the cluster holds no component %s for tenant %s", ErrNotPending, profile, tenantName)
		}
		return DeclineOutcome{}, err
	}
	pending := map[string]bool{}
	for _, ref := range comp.Status.PendingPrivileges {
		pending[ref] = true
	}
	if !pending[privilege] {
		return DeclineOutcome{}, fmt.Errorf("%w: %s waits for %s", ErrNotPending, profile, orNone(comp.Status.PendingPrivileges))
	}
	requests := requestsOf(&comp)
	for ref := range requests {
		if !pending[ref] {
			delete(requests, ref)
		}
	}
	entry := requests[privilege]
	entry.DeclinedBy, entry.DeclinedAt, entry.DeclineReason = actor, time.Now().UTC().Format(time.RFC3339), reason
	requests[privilege] = entry
	if err := s.writeRequests(ctx, &comp, requests); err != nil {
		return DeclineOutcome{}, fmt.Errorf("record the decline on component %s: %w", comp.Name, err)
	}
	return DeclineOutcome{
		Status: "declined", Tenant: tenantName, Profile: profile, Install: comp.Name, Privilege: privilege, Request: entry,
	}, nil
}

func orNone(refs []string) string {
	if len(refs) == 0 {
		return "nothing"
	}
	return strings.Join(refs, ", ")
}

// Approval is one privilege awaiting the security officer, across the
// cluster: what a console or the CLI lists for them.
type Approval struct {
	Tenant  string `json:"tenant"`
	Profile string `json:"profile"`
	// Install is the Component's name: what the approval route names.
	Install string `json:"install"`
	PendingPrivilege
}

// PendingApprovals lists every privilege of every tenant's apps that waits
// for the security officer, in tenant, app and privilege order.
func (s *Service) PendingApprovals(ctx context.Context) ([]Approval, error) {
	var tenants gentianov1alpha1.TenantList
	if err := s.client.List(ctx, &tenants); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	out := []Approval{}
	now := time.Now()
	for i := range tenants.Items {
		tenant := tenants.Items[i].Name
		var comps gentianov1alpha1.ComponentList
		if err := s.client.List(ctx, &comps, client.InNamespace(layout.Tenant(tenant))); err != nil {
			return nil, fmt.Errorf("list the components of tenant %s: %w", tenant, err)
		}
		for j := range comps.Items {
			comp := &comps.Items[j]
			if len(comp.Status.PendingPrivileges) == 0 {
				continue
			}
			profile := &gentianov1alpha1.ComponentProfile{}
			if err := s.client.Get(ctx, client.ObjectKey{Name: comp.Spec.ProfileRef.Name}, profile); err != nil {
				if !apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("get componentprofile %q: %w", comp.Spec.ProfileRef.Name, err)
				}
				profile = nil
			}
			for _, p := range pendingApprovals(comp, profile, now) {
				if p.ApprovedBy != security.PrivilegeApproverSecurityOfficer {
					continue
				}
				out = append(out, Approval{Tenant: tenant, Profile: comp.Spec.ProfileRef.Name, Install: comp.Name, PendingPrivilege: p})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Tenant != out[b].Tenant {
			return out[a].Tenant < out[b].Tenant
		}
		if out[a].Install != out[b].Install {
			return out[a].Install < out[b].Install
		}
		return out[a].Privilege < out[b].Privilege
	})
	return out, nil
}

func (h *HTTPServer) registerApprovalRoutes(mux router) {
	// What awaits the security officer, cluster-wide. A read, and the
	// usher's: the console that lists it is theirs.
	mux.Read("GET /v1/approvals", h.handleApprovals)
	// Asking is an action on the tenant's app: the director names the person
	// who asked. Declining is the approver's answer that changes nothing;
	// the director names them too, having checked they could have approved.
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/request-privilege", h.handleRequestPrivilege)
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/decline-privilege", h.handleDeclinePrivilege)
}

func (h *HTTPServer) handleApprovals(w http.ResponseWriter, r *http.Request) {
	approvals, err := h.Service.PendingApprovals(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": approvals})
}

func (h *HTTPServer) handleRequestPrivilege(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Profile   string `json:"profile"`
		Privilege string `json:"privilege"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil || body.Profile == "" || body.Privilege == "" {
		writeErr(w, http.StatusBadRequest, errors.New(`body must be {"profile": "<name>", "privilege": "<kind>/<name>", "note": "..."}`))
		return
	}
	out, err := h.Service.RequestPrivilege(r.Context(), r.PathValue("tenant"), body.Profile, body.Privilege, body.Note, actorOf(r))
	switch {
	case errors.Is(err, ErrNotPending):
		writeJSON(w, http.StatusConflict, map[string]string{"detail": err.Error(), "reason": "not-pending"})
	case errors.Is(err, ErrTenantAnswers):
		writeJSON(w, http.StatusConflict, map[string]string{"detail": err.Error(), "reason": "tenant-approves"})
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
	case out.Status == "unchanged":
		writeJSON(w, http.StatusOK, out)
	default:
		writeJSON(w, http.StatusAccepted, out)
	}
}

func (h *HTTPServer) handleDeclinePrivilege(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Profile   string `json:"profile"`
		Privilege string `json:"privilege"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil || body.Profile == "" || body.Privilege == "" {
		writeErr(w, http.StatusBadRequest, errors.New(`body must be {"profile": "<name>", "privilege": "<kind>/<name>", "reason": "..."}`))
		return
	}
	out, err := h.Service.DeclinePrivilege(r.Context(), r.PathValue("tenant"), body.Profile, body.Privilege, body.Reason, actorOf(r))
	switch {
	case errors.Is(err, ErrNotPending):
		writeJSON(w, http.StatusConflict, map[string]string{"detail": err.Error(), "reason": "not-pending"})
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusAccepted, out)
	}
}
