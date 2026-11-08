/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// What the director commits is pruned on apply by a cluster whose resource
// definitions are older than the software. The director cannot look at the
// cluster; the operator tells it. These tests put a stand-in for that answer
// behind the real routes and a real repository.

// operatorAnswer is what the operator says the cluster serves, changeable
// while the director runs.
type operatorAnswer struct {
	mu     sync.Mutex
	report schemacheck.Report
	err    error
}

func (o *operatorAnswer) source() (schemacheck.Report, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.report, o.err
}

// current answers that the cluster serves everything, except the fields
// named as missing per kind.
func (o *operatorAnswer) serves(missing map[string][]string) {
	set := schemacheck.Embedded()
	r := schemacheck.Report{Checked: true, OK: len(missing) == 0, Definitions: set.Digest}
	for _, def := range set.Definitions {
		k := schemacheck.KindReport{Kind: def.Kind, CRD: def.CRD, Source: def.Source, XRD: def.XRD, State: schemacheck.StateOK}
		if fields, ok := missing[def.Kind]; ok {
			k.State, k.Missing, k.Remedy = schemacheck.StateOutdated, fields, def.Remedy()
		}
		r.Kinds = append(r.Kinds, k)
	}
	o.mu.Lock()
	o.report, o.err = r, nil
	o.mu.Unlock()
}

func (o *operatorAnswer) unreachable() {
	o.mu.Lock()
	o.report, o.err = schemacheck.Report{}, errors.New("dial tcp: connection refused")
	o.mu.Unlock()
}

func startGuarded(t *testing.T) (*harness, *operatorAnswer) {
	t.Helper()
	operator := &operatorAnswer{}
	operator.serves(nil)
	h := startWith(t, nil, func(cfg *api.Config) {
		cfg.Repo.(*gitops.GitOps).GuardDefinitions(operator.source)
	})
	return h, operator
}

func refused(t *testing.T, what string, code int, body map[string]any, names ...string) {
	t.Helper()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("%s: want 503, got %d %v", what, code, body)
	}
	message := fmt.Sprint(body["error"])
	for _, name := range names {
		if !strings.Contains(message, name) {
			t.Errorf("%s: the refusal does not name %q: %s", what, name, message)
		}
	}
}

// With the cluster current, the check is not in anybody's way.
func TestACurrentClusterIsWrittenAsBefore(t *testing.T) {
	h, _ := startGuarded(t)
	tom := h.token(t, "tenant-demo", "tom")
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, ""); code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, body)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar"]}`); code != http.StatusAccepted {
		t.Fatalf("addons: %d %v", code, body)
	}
}

// The write that needs a field the cluster would drop is refused, with the
// definition, the field and the remedy named and nothing committed. Every
// other write, and every read, is served.
func TestAWriteThatNeedsADroppedFieldIsRefusedAndTheRestServed(t *testing.T) {
	h, operator := startGuarded(t)
	tom := h.token(t, "tenant-demo", "tom")
	alice := h.token(t, "gentian", "alice")
	operator.serves(map[string][]string{"Tenant": {"spec.apps[].addons"}})
	before := h.tip(t)

	code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar"]}`)
	refused(t, "choosing addons", code, body,
		"Tenant", "tenants.gentianos.io", "spec.apps[].addons", "sync the gentian-os Application")
	if h.tip(t) != before {
		t.Fatal("a refused write moved the repository")
	}
	if file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(file, "calendar") {
		t.Fatalf("the refused field reached git:\n%s", file)
	}

	// The same tenant, a write that does not touch the field.
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, ""); code != http.StatusAccepted {
		t.Fatalf("an install that needs no missing field: %d %v", code, body)
	}
	// Another kind altogether.
	if code, body := h.do(t, "PATCH", "/v1/clusters/"+dt.Cluster+"/settings", alice,
		`{"settings":{"mail.serviceMode":"external"}}`); code != http.StatusAccepted {
		t.Fatalf("a cluster setting: %d %v", code, body)
	}
	// A removal, and a read.
	if code, body := h.do(t, "DELETE", "/v1/tenants/demo/apps/element", tom, ""); code != http.StatusAccepted {
		t.Fatalf("an uninstall: %d %v", code, body)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/apps", tom, ""); code != http.StatusOK {
		t.Fatalf("a read: %d", code)
	}

	// The refused write goes through once the definition is updated; nothing
	// of the refusal is left behind in the checkout.
	operator.serves(nil)
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar"]}`); code != http.StatusAccepted {
		t.Fatalf("after the definition was updated: %d %v", code, body)
	}
}

// A kind Crossplane generates is refused with the installer step that
// delivers it.
func TestAClusterClaimFieldTheClusterWouldDropNamesTheInstallerStep(t *testing.T) {
	h, operator := startGuarded(t)
	alice := h.token(t, "gentian", "alice")
	operator.serves(map[string][]string{"Cluster": {"spec.mail"}})
	before := h.tip(t)

	code, body := h.do(t, "PATCH", "/v1/clusters/"+dt.Cluster+"/settings", alice, `{"settings":{"mail.serviceMode":"external"}}`)
	refused(t, "a cluster setting", code, body, "Cluster", "clusters.gentianos.io", "spec.mail.serviceMode", "./install.sh --only B-06")
	if h.tip(t) != before {
		t.Fatal("a refused write moved the repository")
	}
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", h.token(t, "tenant-demo", "tom"), ""); code != http.StatusAccepted {
		t.Fatalf("a tenant write is not affected by the Cluster definition: %d %v", code, body)
	}
}

// Fail closed. A director that cannot learn what the cluster serves does not
// assume it serves everything: the writes that set a field are refused, saying
// the state could not be confirmed, and resume when the operator answers.
func TestWithNoAnswerFromTheOperatorNothingThatSetsAFieldIsWritten(t *testing.T) {
	h, operator := startGuarded(t)
	tom := h.token(t, "tenant-demo", "tom")
	operator.unreachable()
	before := h.tip(t)

	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, "")
	refused(t, "an install", code, body, "could not be confirmed", "operator could not be asked")
	code, body = h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar"]}`)
	refused(t, "choosing addons", code, body, "could not be confirmed")
	code, body = h.do(t, "PATCH", "/v1/clusters/"+dt.Cluster+"/settings", h.token(t, "gentian", "alice"), `{"settings":{"mail.serviceMode":"external"}}`)
	refused(t, "a cluster setting", code, body, "could not be confirmed")
	if h.tip(t) != before {
		t.Fatal("a refused write moved the repository")
	}

	// What can lose nothing is served: a read, and a removal.
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/apps", tom, ""); code != http.StatusOK {
		t.Fatalf("a read: %d", code)
	}
	if code, body := h.do(t, "DELETE", "/v1/tenants/demo/apps/nextcloud", tom, ""); code != http.StatusAccepted {
		t.Fatalf("an uninstall: %d %v", code, body)
	}

	// An operator that answers but has not read the cluster yet, or was built
	// with other definitions, has confirmed nothing either.
	operator.mu.Lock()
	operator.report, operator.err = schemacheck.Report{Definitions: schemacheck.Embedded().Digest}, nil
	operator.mu.Unlock()
	code, body = h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, "")
	refused(t, "an install before the first check", code, body, "could not be confirmed", "has not compared them yet")

	operator.serves(nil)
	operator.mu.Lock()
	operator.report.Definitions = "sha256:another-build"
	operator.mu.Unlock()
	code, body = h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, "")
	refused(t, "an install during a rollout", code, body, "could not be confirmed", "other definitions")

	operator.serves(nil)
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, ""); code != http.StatusAccepted {
		t.Fatalf("once the operator answers: %d %v", code, body)
	}
}
