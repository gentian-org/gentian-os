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

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

const sharesPath = "/v1/tenants/demo/exposures/nextcloud/shares"

func publishBody(expires time.Time) string {
	return fmt.Sprintf(`{"expiresAt":%q,"reason":"the team shares calendars with the client"}`,
		expires.UTC().Format(time.RFC3339))
}

// Publishing is its own decision, asked as its own relation, and recorded as
// its own commit.
func TestAPerimeterApproverPublishesASurface(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(30*24*time.Hour)))
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	// The commit names can_expose, not admin: putting something on the
	// internet is never something that merely happened while administering.
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(trailer, "can_expose tenant:demo") {
		t.Fatalf("the commit does not name can_expose:\n%s", trailer)
	}

	code, body = h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %v", code, body)
	}
	live, _ := body["live"].([]any)
	if len(live) != 1 {
		t.Fatalf("live = %v", body["live"])
	}
	got, _ := live[0].(map[string]any)
	if got["install"] != "nextcloud" || got["exposureName"] != "shares" {
		t.Fatalf("entry = %v", got)
	}
	// The owner is the token's subject, never the body's.
	if got["owner"] != "tom" {
		t.Fatalf("owner = %v, want the caller", got["owner"])
	}
}

// A public surface with no end is not a decision somebody made, so there is
// no way to ask for one.
func TestAPublishedSurfaceIsAlwaysBounded(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")

	// Saying nothing still produces an expiry.
	if code, _ := h.do(t, "PUT", sharesPath, tom, `{"reason":"shared calendars"}`); code != http.StatusAccepted {
		t.Fatalf("PUT with no expiry = %d", code)
	}
	code, body := h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	live, _ := body["live"].([]any)
	entry, _ := live[0].(map[string]any)
	if entry["expiresAt"] == "" || entry["expiresAt"] == nil {
		t.Fatalf("no expiry was recorded: %v (code %d)", entry, code)
	}

	// Ten years is refused rather than quietly clamped: somebody asking for
	// it should be told no.
	long := publishBody(time.Now().Add(10 * 365 * 24 * time.Hour))
	if code, out := h.do(t, "PUT", sharesPath, tom, long); code != http.StatusBadRequest {
		t.Fatalf("a ten-year exposure = %d %v, want 400", code, out)
	}
	// And one already over grants nothing.
	past := publishBody(time.Now().Add(-time.Hour))
	if code, _ := h.do(t, "PUT", sharesPath, tom, past); code != http.StatusBadRequest {
		t.Fatalf("an expiry in the past = %d, want 400", code)
	}
}

// Withdrawing takes it down, and the URL stops answering once the operator
// has seen the commit.
func TestWithdrawingRemovesItFromTheRegistry(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")

	if code, _ := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(24*time.Hour))); code != http.StatusAccepted {
		t.Fatal("could not publish")
	}
	if code, body := h.do(t, "DELETE", sharesPath, tom, ""); code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %v", code, body)
	}
	_, body := h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	if live, _ := body["live"].([]any); len(live) != 0 {
		t.Fatalf("still published: %v", live)
	}
	// Withdrawing what is not published changes nothing and says so.
	if code, _ := h.do(t, "DELETE", sharesPath, tom, ""); code != http.StatusOK {
		t.Fatalf("a second withdrawal = %d, want 200 unchanged", code)
	}
}

// Administering a tenant is not publishing from it. alice runs the cluster
// and mia is a member; neither holds can_expose.
func TestPublishingNeedsTheExposeRelation(t *testing.T) {
	h := start(t, false)
	before := h.tip(t)

	for name, tok := range map[string]string{
		"a member":             h.token(t, "tenant-demo", "mia"),
		"a cluster configurer": h.token(t, "gentian", "alice"),
	} {
		code, _ := h.do(t, "PUT", sharesPath, tok, publishBody(time.Now().Add(24*time.Hour)))
		if code != http.StatusForbidden {
			t.Errorf("%s published a surface: %d", name, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused publish committed anyway")
	}
}

// A perimeter approver who is not an administrator may publish. pat holds
// can_expose and nothing else, which is the model's own fixture for the role
// existing separately from running the tenant.
func TestAnApproverNeedNotBeAnAdministrator(t *testing.T) {
	h := start(t, false)
	pat := h.token(t, "tenant-demo", "pat")

	if code, body := h.do(t, "PUT", sharesPath, pat, publishBody(time.Now().Add(24*time.Hour))); code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	// Reading the registry is can_view, which pat does not hold: publishing
	// and seeing the tenant are different questions, and pat was given one.
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/exposures", pat, ""); code != http.StatusForbidden {
		t.Fatalf("GET = %d; reading the tenant's registry is can_view", code)
	}
}
