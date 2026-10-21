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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// crmProfile asks for one thing of each approver, with its reasons.
func crmProfile() *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "crm"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{
				Privileges: &gentianov1alpha1.PrivilegeRequest{
					PodSecurity: []gentianov1alpha1.PodSecurityWaiver{{
						Name: "run-as-root", Policy: "gentian-require-non-root", Scope: "web",
						Reason: "the vendor image writes its cache as root",
					}},
					Egress: []gentianov1alpha1.EgressRequest{{
						Name: "smtp-relay", Reason: "sends mail through the tenant's relay",
					}},
				},
			},
		},
	}
}

func waitingCRM() *gentianov1alpha1.Component {
	crm := component("crm", "PrivilegesPending", "waiting for approval", false)
	crm.Status.PendingPrivileges = []string{"egress/smtp-relay", "podSecurity/run-as-root"}
	return crm
}

// Each pending request carries the profile's own reason, and the request a
// tenant administrator made of the security officer once there is one.
func TestAPendingPrivilegeCarriesItsReasonAndItsRequest(t *testing.T) {
	s := appsService(t, demoTenant(), crmProfile(), waitingCRM())
	ctx := context.Background()

	states, err := s.AppStates(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	before := byProfile(t, states)["crm"].PendingPrivilegeApprovals
	if len(before) != 2 || before[0].Reason != "sends mail through the tenant's relay" ||
		before[1].Reason != "the vendor image writes its cache as root" || before[1].RequestedBy != "" {
		t.Fatalf("before any request: %+v", before)
	}

	out, err := s.RequestPrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", "  needed for the pilot  ", "tom@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "requested" || out.Install != "crm" || out.Request.RequestedBy != "tom@example.com" ||
		out.Request.Note != "needed for the pilot" || out.Request.RequestedAt == "" {
		t.Fatalf("request = %+v", out)
	}
	if _, err := time.Parse(time.RFC3339, out.Request.RequestedAt); err != nil {
		t.Fatalf("requestedAt = %q: %v", out.Request.RequestedAt, err)
	}

	states, err = s.AppStates(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	after := byProfile(t, states)["crm"].PendingPrivilegeApprovals
	if after[1].RequestedBy != "tom@example.com" || after[1].RequestedAt != out.Request.RequestedAt || after[1].Note != "needed for the pilot" {
		t.Fatalf("after the request: %+v", after[1])
	}
	if after[0].RequestedBy != "" {
		t.Fatalf("the egress request, which nobody asked for, carries %+v", after[0])
	}

	// Asking again records the first asking, whoever asks.
	again, err := s.RequestPrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", "please", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "unchanged" || again.Request != out.Request {
		t.Fatalf("a second request = %+v, want the first", again)
	}
}

// What cannot be asked for: a privilege the app does not wait for, one the
// tenant answers itself, and one of an app the cluster does not hold.
func TestWhatCannotBeRequested(t *testing.T) {
	s := appsService(t, demoTenant(), crmProfile(), waitingCRM())
	ctx := context.Background()
	for name, c := range map[string]struct {
		profile, privilege string
		want               error
	}{
		"not pending":  {"crm", "podSecurity/keep-capabilities", ErrNotPending},
		"tenant's own": {"crm", "egress/smtp-relay", ErrTenantAnswers},
		"no such app":  {"wiki", "podSecurity/run-as-root", ErrNotPending},
	} {
		_, err := s.RequestPrivilege(ctx, "demo", c.profile, c.privilege, "", "tom@example.com")
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if _, err := s.RequestPrivilege(ctx, "demo", "crm", "run-as-root", "", "tom@example.com"); err == nil {
		t.Error("a privilege that is not <kind>/<name> was accepted")
	}
	if _, err := s.RequestPrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", strings.Repeat("x", noteLimit+1), "tom@example.com"); err == nil {
		t.Error("a note beyond the limit was accepted")
	}
	var crm gentianov1alpha1.Component
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: "tenant-demo", Name: "crm"}, &crm); err != nil {
		t.Fatal(err)
	}
	if crm.Annotations[privilegeRequestsAnnotation] != "" {
		t.Fatalf("a refused request was recorded: %s", crm.Annotations[privilegeRequestsAnnotation])
	}
}

// The cluster's list is every tenant's requests of the security officer and
// nothing a tenant answers itself, with a grant that ran out shown as such.
func TestTheClusterListsWhatAwaitsTheSecurityOfficer(t *testing.T) {
	crm := waitingCRM()
	crm.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
		Privilege: "podSecurity/run-as-root", Approver: "sam", Reason: "for the pilot, until March",
		ApprovedAt: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		ExpiresAt:  &metav1.Time{Time: time.Now().Add(-time.Hour)},
	}}
	other := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	wiki := component("wiki", "PrivilegesPending", "waiting", false)
	wiki.Namespace = "tenant-other"
	wiki.Status.PendingPrivileges = []string{"clusterRoles/node-reader"}
	quiet := component("notes", "Ready", "release deployed", true)
	s := appsService(t, demoTenant(), other, crmProfile(), crm, wiki, quiet)
	ctx := context.Background()
	if _, err := s.RequestPrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", "", "tom@example.com"); err != nil {
		t.Fatal(err)
	}

	list, err := s.PendingApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("approvals = %+v", list)
	}
	if list[0].Tenant != "demo" || list[0].Install != "crm" || list[0].Privilege != "podSecurity/run-as-root" ||
		list[0].ApprovedBy != security.PrivilegeApproverSecurityOfficer ||
		list[0].Reason != "the vendor image writes its cache as root" || list[0].RequestedBy != "tom@example.com" ||
		list[0].ExpiredAt == "" {
		t.Errorf("demo's = %+v", list[0])
	}
	// A profile the cluster no longer has: listed all the same, without a
	// reason to show.
	if list[1].Tenant != "other" || list[1].Privilege != "clusterRoles/node-reader" || list[1].Reason != "" || list[1].RequestedBy != "" {
		t.Errorf("other's = %+v", list[1])
	}
}

// The list is a read the usher's identity is admitted to; the request is the
// one command, and it records the actor the director named.
func TestTheApprovalRoutes(t *testing.T) {
	s := appsService(t, demoTenant(), crmProfile(), waitingCRM())
	_, auth := cluster()
	mux := (&HTTPServer{Auth: auth, Service: s}).routes()
	call := func(method, path, token, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("X-Gentian-Actor", "tom@example.com")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	for _, token := range []string{"usher", "director"} {
		code, out := call("GET", "/v1/approvals", token, "")
		if list, _ := out["approvals"].([]any); code != http.StatusOK || len(list) != 1 {
			t.Fatalf("the list as %s: %d %v", token, code, out)
		}
	}
	if code, _ := call("GET", "/v1/approvals", "custodian", ""); code != http.StatusForbidden {
		t.Fatalf("the list as the custodian: %d", code)
	}

	request := "/v1/tenants/demo/actions/request-privilege"
	body := `{"profile":"crm","privilege":"podSecurity/run-as-root","note":"for the pilot"}`
	if code, _ := call("POST", request, "usher", body); code != http.StatusForbidden {
		t.Fatalf("the usher asked for a privilege: %d", code)
	}
	if code, _ := call("POST", request, "director", `{"profile":"crm"}`); code != http.StatusBadRequest {
		t.Fatalf("half a request: %d", code)
	}
	code, out := call("POST", request, "director", body)
	req, _ := out["request"].(map[string]any)
	if code != http.StatusAccepted || out["status"] != "requested" || req["requestedBy"] != "tom@example.com" || req["note"] != "for the pilot" {
		t.Fatalf("the request: %d %v", code, out)
	}
	if code, out := call("POST", request, "director", body); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("the request again: %d %v", code, out)
	}
	code, out = call("POST", request, "director", `{"profile":"crm","privilege":"egress/smtp-relay"}`)
	if code != http.StatusConflict || out["reason"] != "tenant-approves" {
		t.Fatalf("the tenant's own: %d %v", code, out)
	}
	code, out = call("POST", request, "director", `{"profile":"crm","privilege":"podSecurity/other"}`)
	if code != http.StatusConflict || out["reason"] != "not-pending" {
		t.Fatalf("not pending: %d %v", code, out)
	}

	decline := "/v1/tenants/demo/actions/decline-privilege"
	body = `{"profile":"crm","privilege":"podSecurity/run-as-root","reason":"no root on shared nodes"}`
	if code, _ := call("POST", decline, "usher", body); code != http.StatusForbidden {
		t.Fatalf("the usher declined a privilege: %d", code)
	}
	if code, _ := call("POST", decline, "director", `{"profile":"crm","privilege":"podSecurity/run-as-root","reason":"no"}`); code != http.StatusBadRequest {
		t.Fatalf("a decline without a reason: %d", code)
	}
	code, out = call("POST", decline, "director", body)
	req, _ = out["request"].(map[string]any)
	if code != http.StatusAccepted || out["status"] != "declined" || req["declinedBy"] != "tom@example.com" || req["declineReason"] != "no root on shared nodes" {
		t.Fatalf("the decline: %d %v", code, out)
	}
	code, out = call("POST", decline, "director", `{"profile":"crm","privilege":"podSecurity/other","reason":"no root on shared nodes"}`)
	if code != http.StatusConflict || out["reason"] != "not-pending" {
		t.Fatalf("declining what is not pending: %d %v", code, out)
	}
	if _, listed := call("GET", "/v1/approvals", "usher", ""); listed["approvals"].([]any)[0].(map[string]any)["declinedBy"] != "tom@example.com" {
		t.Fatalf("the list after the decline: %v", listed)
	}
}

// A decline is recorded with who, when and why, shown on the app's state and
// on the cluster's list, and installs nothing: the privilege stays pending.
// A new request clears it; so does an approval made after it.
func TestADeclineIsRecordedAndCleared(t *testing.T) {
	s := appsService(t, demoTenant(), crmProfile(), waitingCRM())
	ctx := context.Background()
	if _, err := s.RequestPrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", "for the pilot", "tom@example.com"); err != nil {
		t.Fatal(err)
	}
	out, err := s.DeclinePrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", "  no root on shared nodes; use the rootless image  ", "sam@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "declined" || out.Install != "crm" || out.Request.DeclinedBy != "sam@example.com" ||
		out.Request.DeclineReason != "no root on shared nodes; use the rootless image" || out.Request.RequestedBy != "tom@example.com" {
		t.Fatalf("decline = %+v", out)
	}
	if _, err := time.Parse(time.RFC3339, out.Request.DeclinedAt); err != nil {
		t.Fatalf("declinedAt = %q: %v", out.Request.DeclinedAt, err)
	}

	states, err := s.AppStates(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	crm := byProfile(t, states)["crm"]
	if len(crm.PendingPrivileges) != 2 {
		t.Fatalf("a decline took the privilege off the pending list: %v", crm.PendingPrivileges)
	}
	got := crm.PendingPrivilegeApprovals[1]
	if got.DeclinedBy != "sam@example.com" || got.DeclinedAt != out.Request.DeclinedAt ||
		got.DeclineReason != out.Request.DeclineReason || got.RequestedBy != "tom@example.com" {
		t.Fatalf("after the decline: %+v", got)
	}
	list, err := s.PendingApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].DeclinedBy != "sam@example.com" || list[0].DeclineReason == "" {
		t.Fatalf("the cluster's list: %+v", list)
	}

	// Asking again after a no is a new request, not the first one repeated,
	// and it clears the decline.
	again, err := s.RequestPrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", "now with the rootless image", "tom@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "requested" || again.Request.Note != "now with the rootless image" || again.Request.declined() {
		t.Fatalf("the request after a decline = %+v", again)
	}
	states, _ = s.AppStates(ctx, "demo")
	if got := byProfile(t, states)["crm"].PendingPrivilegeApprovals[1]; got.DeclinedBy != "" || got.Note != "now with the rootless image" {
		t.Fatalf("after asking again: %+v", got)
	}

	// Declined once more, then approved: the grant stands and the decline
	// does not show, even while the entry is still on the annotation and
	// the grant has since run out.
	if _, err := s.DeclinePrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", "still no; the image still runs as root", "sam@example.com"); err != nil {
		t.Fatal(err)
	}
	var comp gentianov1alpha1.Component
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: "tenant-demo", Name: "crm"}, &comp); err != nil {
		t.Fatal(err)
	}
	comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
		Privilege: "podSecurity/run-as-root", Approver: "sam", Reason: "reviewed; for the pilot only",
		ApprovedAt: metav1.NewTime(time.Now().Add(time.Minute)),
		ExpiresAt:  &metav1.Time{Time: time.Now().Add(-time.Second)},
	}}
	if err := s.client.Update(ctx, &comp); err != nil {
		t.Fatal(err)
	}
	states, _ = s.AppStates(ctx, "demo")
	if got := byProfile(t, states)["crm"].PendingPrivilegeApprovals[1]; got.DeclinedBy != "" || got.ExpiredAt == "" {
		t.Fatalf("after an approval: %+v", got)
	}
}

// What cannot be declined, and a decline of something nobody asked for.
func TestWhatCannotBeDeclined(t *testing.T) {
	s := appsService(t, demoTenant(), crmProfile(), waitingCRM())
	ctx := context.Background()
	good := "no root on shared nodes"
	for name, c := range map[string]struct {
		profile, privilege, reason string
		want                       error
	}{
		"not pending": {"crm", "podSecurity/keep-capabilities", good, ErrNotPending},
		"no such app": {"wiki", "podSecurity/run-as-root", good, ErrNotPending},
	} {
		if _, err := s.DeclinePrivilege(ctx, "demo", c.profile, c.privilege, c.reason, "sam@example.com"); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	for name, reason := range map[string]string{"none": "", "short": "   nope   ", "endless": strings.Repeat("x", noteLimit+1)} {
		if _, err := s.DeclinePrivilege(ctx, "demo", "crm", "podSecurity/run-as-root", reason, "sam@example.com"); err == nil {
			t.Errorf("a %s reason was accepted", name)
		}
	}
	if _, err := s.DeclinePrivilege(ctx, "demo", "crm", "run-as-root", good, "sam@example.com"); err == nil {
		t.Error("a privilege that is not <kind>/<name> was accepted")
	}
	var crm gentianov1alpha1.Component
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: "tenant-demo", Name: "crm"}, &crm); err != nil {
		t.Fatal(err)
	}
	if crm.Annotations[privilegeRequestsAnnotation] != "" {
		t.Fatalf("a refused decline was recorded: %s", crm.Annotations[privilegeRequestsAnnotation])
	}

	// Nobody asked, and the tenant's own kind: declined all the same, since
	// the director let whoever could have approved it say no.
	out, err := s.DeclinePrivilege(ctx, "demo", "crm", "egress/smtp-relay", "the relay is not for this app", "tom@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if out.Request.RequestedBy != "" || out.Request.DeclinedBy != "tom@example.com" {
		t.Fatalf("decline = %+v", out)
	}
	states, err := s.AppStates(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := byProfile(t, states)["crm"].PendingPrivilegeApprovals[0]; got.Privilege != "egress/smtp-relay" || got.DeclinedBy != "tom@example.com" {
		t.Fatalf("the egress entry: %+v", got)
	}
}
