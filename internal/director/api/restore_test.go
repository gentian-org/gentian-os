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
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/backup"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// restoringOperator answers inspect with a manifest of the tenant it is told,
// and records every restore it is asked to start.
type restoringOperator struct {
	mu sync.Mutex
	// of is the tenant the bundle's manifest names.
	of       string
	inspects []string
	restores []string
}

func (o *restoringOperator) Get(context.Context, string, url.Values) (int, []byte, error) {
	return http.StatusNotFound, []byte(`{}`), nil
}

func (o *restoringOperator) Do(_ context.Context, path, _ string, body any) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	raw, _ := json.Marshal(body)
	switch {
	case path == "/v1/bundles/inspect":
		o.inspects = append(o.inspects, string(raw))
		if !strings.Contains(string(raw), `"passphrase":"open sesame"`) {
			return http.StatusBadRequest, []byte(`{"detail":"the manifest could not be opened with that key"}`), nil
		}
		return http.StatusOK, []byte(`{"bundle":{"bucket":"b","prefix":"p","credentialSecret":"x"},"manifest":{"schemaVersion":4,"tenant":"` +
			o.of + `","export":"nightly","createdAt":"2026-10-01T03:00:00Z","apps":[]}}`), nil
	case strings.HasSuffix(path, "/actions/restore"):
		o.restores = append(o.restores, path+" "+string(raw))
		return http.StatusAccepted, []byte(`{"name":"restore-1","tenant":"x","phase":""}`), nil
	}
	return http.StatusNotFound, nil, nil
}

func (o *restoringOperator) Upload(_ context.Context, _, _ string, body io.Reader) (int, []byte, error) {
	_, _ = io.Copy(io.Discard, body)
	return http.StatusCreated, []byte(`{"bundle":{"bucket":"gentian-imports","prefix":"u"}}`), nil
}

func (o *restoringOperator) Stream(context.Context, string) (*http.Response, error) {
	return nil, io.EOF
}

func (o *restoringOperator) Plans(context.Context, string, bool) ([]lifecycle.Plan, error) {
	return nil, nil
}

func (o *restoringOperator) asked() ([]string, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.inspects...), append([]string(nil), o.restores...)
}

const restoreKey = `"decryption":{"passphrase":"open sesame"}`

// A tenant's administrator puts one of its own backups back into it: the
// operator is asked to restore that backup by name, in that tenant, with the
// key, and nothing is committed.
func TestATenantsAdministratorRestoresItsOwnBackup(t *testing.T) {
	op := &restoringOperator{of: "demo"}
	h := startWith(t, op)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	code, body := h.do(t, "POST", "/v1/tenants/demo/actions/restore", tom, `{"backup":"nightly",`+restoreKey+`}`)
	if code != http.StatusAccepted || body["name"] != "restore-1" {
		t.Fatalf("restore = %d %v", code, body)
	}
	inspects, restores := op.asked()
	if len(inspects) != 1 || !strings.Contains(inspects[0], `"tenant":"demo"`) || !strings.Contains(inspects[0], `"backup":"nightly"`) {
		t.Fatalf("inspected %v", inspects)
	}
	if len(restores) != 1 || !strings.HasPrefix(restores[0], "/v1/tenants/demo/actions/restore ") ||
		!strings.Contains(restores[0], `"backup":"nightly"`) || strings.Contains(restores[0], `"bundle"`) {
		t.Fatalf("restores %v", restores)
	}
	if h.tip(t) != before {
		t.Fatal("a restore committed something")
	}

	// The inspection answers the manifest and nothing of where the bundle is.
	code, body = h.do(t, "POST", "/v1/tenants/demo/bundles/inspect", tom, `{"backup":"nightly",`+restoreKey+`}`)
	if code != http.StatusOK || body["manifest"] == nil || body["bundle"] != nil {
		t.Fatalf("inspect = %d %v", code, body)
	}

	// Which version line this platform is of, for the tenant's administrator,
	// who may not read the cluster's settings.
	if code, body := h.do(t, "GET", "/v1/tenants/demo/me", tom, ""); code != http.StatusOK || body["platformVersionLine"] != backup.PlatformVersionLine {
		t.Fatalf("me = %d %v", code, body)
	}
}

// A bundle of another tenant is refused before a restore is asked for, and so
// is a bundle reference that names storage or a credential: an uploaded
// bundle is named by the upload's place in the import bucket, and a backup by
// its name.
func TestARestoreTakesOnlyABundleOfTheTenantFromWhereThePlatformKeepsIt(t *testing.T) {
	op := &restoringOperator{of: "solo"}
	h := startWith(t, op)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "POST", "/v1/tenants/demo/bundles", tom, "a tar")
	if code != http.StatusAccepted {
		t.Fatalf("upload = %d %v", code, body)
	}
	uploaded := `{"bundle":{"bucket":"gentian-imports","prefix":"u"},` + restoreKey + `}`
	if code, body := h.do(t, "POST", "/v1/tenants/demo/actions/restore", tom, uploaded); code != http.StatusUnprocessableEntity {
		t.Fatalf("a bundle of solo into demo = %d %v", code, body)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/bundles/inspect", tom, uploaded); code != http.StatusUnprocessableEntity {
		t.Fatalf("inspecting a bundle of solo from demo = %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/actions/restore", tom, `{"bundle":{"bucket":"gentian-imports","prefix":"u"},"decryption":{"passphrase":"no"}}`); code != http.StatusBadRequest {
		t.Fatalf("the wrong key = %d", code)
	}
	for _, refused := range []string{
		`{"bundle":{"bucket":"solo-gentian-backup","prefix":"nightly"},` + restoreKey + `}`,
		`{"bundle":{"bucket":"gentian-imports","prefix":"u","credentialSecret":"minio-admin"},` + restoreKey + `}`,
		`{"bundle":{"bucket":"gentian-imports","prefix":"u","endpoint":"https://evil.example"},` + restoreKey + `}`,
		`{"backup":"nightly","bundle":{"bucket":"gentian-imports","prefix":"u"},` + restoreKey + `}`,
		`{"backup":"../x",` + restoreKey + `}`,
		`{"backup":"nightly"}`,
	} {
		if code, body := h.do(t, "POST", "/v1/tenants/demo/actions/restore", tom, refused); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v", refused, code, body)
		}
	}
	inspects, restores := op.asked()
	if len(restores) != 0 {
		t.Fatalf("restores started: %v", restores)
	}
	if len(inspects) != 3 {
		t.Fatalf("the operator was asked to open %d bundles, want 3: %v", len(inspects), inspects)
	}
}

// Who may restore: the tenant's administrators on their own tenant and on no
// other, the cluster's administrator through the tenant while it consents to
// being operated and through the cluster's route always, and nobody else.
func TestWhoMayRestoreATenant(t *testing.T) {
	op := &restoringOperator{of: "solo"}
	h := startWith(t, op)
	backup := `{"backup":"nightly",` + restoreKey + `}`
	for _, c := range []struct {
		who, realm, path string
		want             int
	}{
		{"mia", "tenant-demo", "/v1/tenants/demo/actions/restore", http.StatusForbidden},
		{"tom", "tenant-demo", "/v1/tenants/solo/actions/restore", http.StatusForbidden},
		{"tom", "tenant-demo", "/v1/clusters/" + dt.Cluster + "/tenants/solo/actions/restore", http.StatusForbidden},
		{"alice", "gentian", "/v1/tenants/solo/actions/restore", http.StatusForbidden},
		{"alice", "gentian", "/v1/clusters/" + dt.Cluster + "/tenants/solo/actions/restore", http.StatusAccepted},
	} {
		code, body := h.do(t, "POST", c.path, h.token(t, c.realm, c.who), backup)
		if code != c.want {
			t.Errorf("%s %s = %d %v, want %d", c.who, c.path, code, body, c.want)
		}
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/bundles", h.token(t, "tenant-demo", "mia"), "a tar"); code != http.StatusForbidden {
		t.Errorf("mia uploading = %d", code)
	}
	_, restores := op.asked()
	if len(restores) != 1 || !strings.HasPrefix(restores[0], "/v1/tenants/solo/actions/restore ") {
		t.Fatalf("restores %v", restores)
	}
}

// A bundle where the tenant's backups are written, which the cluster no
// longer lists as a backup, is restored by its source and prefix: the
// tenant's own source, named for the tenant in the path and no other, and
// never one of the cluster's.
func TestARestoreNamesABundleAtTheTenantsOwnSource(t *testing.T) {
	op := &restoringOperator{of: "demo"}
	h := startWith(t, op)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "POST", "/v1/tenants/demo/actions/restore", tom,
		`{"source":{"name":"destination","prefix":"nightly-20261001"},`+restoreKey+`}`)
	if code != http.StatusAccepted {
		t.Fatalf("restore = %d %v", code, body)
	}
	inspects, restores := op.asked()
	want := `"source":{"name":"destination","prefix":"nightly-20261001","tenant":"demo"}`
	if len(inspects) != 1 || !strings.Contains(inspects[0], want) {
		t.Fatalf("inspected %v", inspects)
	}
	if len(restores) != 1 || !strings.Contains(restores[0], want) {
		t.Fatalf("restores %v", restores)
	}
	for _, refused := range []string{
		`{"source":{"name":"old-store","prefix":"p"},` + restoreKey + `}`,
		`{"source":{"name":"storage"},` + restoreKey + `}`,
		`{"source":{"name":"storage","prefix":"p"},"backup":"nightly",` + restoreKey + `}`,
	} {
		if code, body := h.do(t, "POST", "/v1/tenants/demo/actions/restore", tom, refused); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v", refused, code, body)
		}
	}
}
