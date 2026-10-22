/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// requestingOperator is the operator's listener as far as asking for a
// privilege goes: it records each request with the actor the director named
// and answers as the real handler does.
type requestingOperator struct {
	mu    sync.Mutex
	asked []string
	// refuse, when set, is the operator's own refusal of the next request.
	refuse string
}

func (o *requestingOperator) requests() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.asked...)
}

func (o *requestingOperator) Get(context.Context, string, url.Values) (int, []byte, error) {
	return http.StatusNotFound, []byte(`{"detail":"no such read"}`), nil
}

func (o *requestingOperator) Plans(context.Context, string, bool) ([]lifecycle.Plan, error) {
	return nil, nil
}

func (o *requestingOperator) Stream(context.Context, string) (*http.Response, error) {
	return nil, errors.New("no streams here")
}

func (o *requestingOperator) Upload(context.Context, string, string, io.Reader) (int, []byte, error) {
	return http.StatusNotFound, nil, nil
}

func (o *requestingOperator) Do(_ context.Context, path, actor string, body any) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	raw, _ := json.Marshal(body)
	o.asked = append(o.asked, actor+" "+path+" "+string(raw))
	if o.refuse != "" {
		return http.StatusConflict, []byte(`{"detail":"` + o.refuse + `","reason":"not-pending"}`), nil
	}
	var req map[string]string
	_ = json.Unmarshal(raw, &req)
	if strings.HasSuffix(path, "/decline-privilege") {
		return http.StatusAccepted, []byte(`{"status":"declined","tenant":"demo","profile":"` + req["profile"] +
			`","install":"` + req["profile"] + `","privilege":"` + req["privilege"] +
			`","request":{"declinedBy":"` + actor + `","declinedAt":"2026-10-21T10:00:00Z","declineReason":"` + req["reason"] + `"}}`), nil
	}
	return http.StatusAccepted, []byte(`{"status":"requested","tenant":"demo","profile":"` + req["profile"] +
		`","install":"` + req["profile"] + `","privilege":"` + req["privilege"] +
		`","request":{"requestedBy":"` + actor + `","requestedAt":"2026-10-21T09:00:00Z","note":"` + req["note"] + `"}}`), nil
}

const requestWaiver = "/v1/tenants/demo/apps/nextcloud/actions/request-privilege"

// Asking the security officer is whoever may install's, as purging is. The
// security officer themselves, who holds no right inside the tenant, does
// not ask; they answer.
func TestWhoeverMayInstallAsksTheSecurityOfficer(t *testing.T) {
	op := &requestingOperator{}
	h := startWith(t, op)
	before := h.tip(t)
	body := `{"privilege":"podSecurity/run-as-root","note":"for the pilot"}`
	for who, token := range map[string]string{
		"a member of the tenant":           h.token(t, "tenant-demo", "mia"),
		"another tenant's administrator":   h.token(t, "tenant-solo", "tina"),
		"the security officer":             h.token(t, "gentian", "sam"),
		"somebody with no relation at all": h.token(t, "tenant-demo", "nobody"),
	} {
		if code, _ := h.do(t, "POST", requestWaiver, token, body); code != http.StatusForbidden {
			t.Errorf("%s asked: %d", who, code)
		}
	}
	if code, _ := h.do(t, "POST", requestWaiver, "", body); code != http.StatusUnauthorized {
		t.Errorf("nobody: %d", code)
	}
	if len(op.requests()) != 0 {
		t.Fatalf("a refused caller reached the operator: %v", op.requests())
	}

	h.asked.reset()
	code, out := h.do(t, "POST", requestWaiver, h.token(t, "tenant-demo", "tom"), body)
	if code != http.StatusAccepted || out["status"] != "requested" {
		t.Fatalf("the tenant's administrator: %d %v", code, out)
	}
	if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:tom can_install_app tenant:demo" {
		t.Errorf("asked %v", asked)
	}
	// The operator was told who asked -- the token's holder, never the body
	// -- and what for.
	want := `tom@example.com /v1/tenants/demo/actions/request-privilege {"note":"for the pilot","privilege":"podSecurity/run-as-root","profile":"nextcloud"}`
	if got := op.requests(); len(got) != 1 || got[0] != want {
		t.Fatalf("the operator was asked %v,\nwant %s", got, want)
	}
	req, _ := out["request"].(map[string]any)
	if req["requestedBy"] != "tom@example.com" {
		t.Fatalf("request = %v", out["request"])
	}
	// Nothing was committed: a request is not desired state.
	if h.tip(t) != before {
		t.Fatal("asking committed something")
	}
}

// What is not a request never reaches the operator, and the operator's own
// refusal comes back as it was worded.
func TestARequestIsCheckedHereAndRefusedThere(t *testing.T) {
	op := &requestingOperator{}
	h := startWith(t, op)
	tom := h.token(t, "tenant-demo", "tom")
	for name, body := range map[string]string{
		"no privilege":  `{"note":"x"}`,
		"no kind":       `{"privilege":"run-as-root"}`,
		"unknown kind":  `{"privilege":"hostNetwork/x"}`,
		"unknown field": `{"privilege":"podSecurity/run-as-root","reason":"x"}`,
	} {
		if code, _ := h.do(t, "POST", requestWaiver, tom, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(op.requests()) != 0 {
		t.Fatalf("a malformed request reached the operator: %v", op.requests())
	}
	op.refuse = "the app is not waiting for that privilege: nextcloud waits for nothing"
	code, out := h.do(t, "POST", requestWaiver, tom, `{"privilege":"podSecurity/run-as-root"}`)
	if code != http.StatusConflict || out["error"] != op.refuse {
		t.Fatalf("the operator's refusal: %d %v", code, out)
	}
}

const declineWaiver = "/v1/tenants/demo/apps/nextcloud/actions/decline-privilege"
const declineEgress = "/v1/tenants/demo/apps/nextcloud/actions/decline-privilege"

// Declining is the same authority as approving, kind by kind: the security
// officer declines a waiver, and the tenant's administrator -- however
// complete their authority inside the tenant -- does not. Nothing is
// committed either way: a decline changes nothing the platform gives the app.
func TestWhoeverMayApproveMayDecline(t *testing.T) {
	op := &requestingOperator{}
	h := startWith(t, op)
	before := h.tip(t)
	waiver := `{"privilege":"podSecurity/run-as-root","reason":"no root on shared nodes; use the rootless image"}`
	for who, token := range map[string]string{
		"the tenant's administrator":       h.token(t, "tenant-demo", "tom"),
		"the cluster's administrator":      h.token(t, "gentian", "alice"),
		"a member of the tenant":           h.token(t, "tenant-demo", "mia"),
		"somebody with no relation at all": h.token(t, "tenant-demo", "nobody"),
	} {
		code, body := h.do(t, "POST", declineWaiver, token, waiver)
		if code != http.StatusForbidden {
			t.Errorf("%s declined a waiver: %d %v", who, code, body)
		}
	}
	if code, body := h.do(t, "POST", declineWaiver, h.token(t, "tenant-demo", "tom"), waiver); code != http.StatusForbidden ||
		!strings.Contains(body["error"].(string), "security officer") {
		t.Errorf("the refusal should say whose this is: %d %v", code, body)
	}
	if len(op.requests()) != 0 {
		t.Fatalf("a refused caller reached the operator: %v", op.requests())
	}

	h.asked.reset()
	code, out := h.do(t, "POST", declineWaiver, h.token(t, "gentian", "sam"), waiver)
	if code != http.StatusAccepted || out["status"] != "declined" {
		t.Fatalf("the security officer: %d %v", code, out)
	}
	// Each asked as the account and, refused, as the role holder of its
	// realm (authz.CheckAs); what decides is the security officer's verb.
	if asked := h.asked.questions(); len(asked) == 0 || !strings.HasSuffix(asked[len(asked)-1], "sam can_approve cluster:demo-cluster") ||
		!strings.Contains(strings.Join(asked, ","), "sam can_view tenant:demo") {
		t.Errorf("asked %v, want the route's can_view and then the security officer's verb", asked)
	}
	// The operator was told who declined -- the token's holder -- and why.
	want := `sam@example.com /v1/tenants/demo/actions/decline-privilege {"privilege":"podSecurity/run-as-root","profile":"nextcloud","reason":"no root on shared nodes; use the rootless image"}`
	if got := op.requests(); len(got) != 1 || got[0] != want {
		t.Fatalf("the operator was told %v,\nwant %s", got, want)
	}
	req, _ := out["request"].(map[string]any)
	if req["declinedBy"] != "sam@example.com" || req["declineReason"] == "" {
		t.Fatalf("request = %v", out["request"])
	}
	if h.tip(t) != before {
		t.Fatal("declining committed something")
	}

	// An egress request is the tenant administrator's to answer, so it is
	// theirs to decline, and the security officer's authority on the cluster
	// is not that.
	egress := `{"privilege":"egress/smtp-relay","reason":"the relay is not for this app"}`
	if code, body := h.do(t, "POST", declineEgress, h.token(t, "gentian", "sam"), egress); code != http.StatusForbidden {
		t.Errorf("the security officer declined an egress: %d %v", code, body)
	}
	if code, body := h.do(t, "POST", declineEgress, h.token(t, "tenant-demo", "tom"), egress); code != http.StatusAccepted {
		t.Errorf("the tenant's administrator: %d %v", code, body)
	}
}

// A decline needs a reason, as a grant does; what is not a decline never
// reaches the operator; and the operator's refusal comes back as worded.
func TestADeclineNeedsAReasonAndAPendingRequest(t *testing.T) {
	op := &requestingOperator{}
	h := startWith(t, op)
	sam := h.token(t, "gentian", "sam")
	for name, body := range map[string]string{
		"no reason":     `{"privilege":"podSecurity/run-as-root"}`,
		"a short one":   `{"privilege":"podSecurity/run-as-root","reason":"no"}`,
		"no privilege":  `{"reason":"no root on shared nodes"}`,
		"unknown kind":  `{"privilege":"hostNetwork/x","reason":"no root on shared nodes"}`,
		"unknown field": `{"privilege":"podSecurity/run-as-root","reason":"no root on shared nodes","note":"x"}`,
	} {
		if code, _ := h.do(t, "POST", declineWaiver, sam, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(op.requests()) != 0 {
		t.Fatalf("a malformed decline reached the operator: %v", op.requests())
	}
	op.refuse = "the app is not waiting for that privilege: nextcloud waits for nothing"
	code, out := h.do(t, "POST", declineWaiver, sam, `{"privilege":"podSecurity/run-as-root","reason":"no root on shared nodes"}`)
	if code != http.StatusConflict || out["error"] != op.refuse {
		t.Fatalf("the operator's refusal: %d %v", code, out)
	}
}
