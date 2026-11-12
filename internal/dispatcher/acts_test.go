/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/tokenreview"
)

const directorName = "system:serviceaccount:kernel-control:gentian-os-director"

// tokens says whose each token is, as the API server would.
type tokens map[string]string

func (tokens) Ready() bool { return true }
func (t tokens) Identify(_ context.Context, token string) (string, error) {
	switch name, known := t[token]; {
	case token == "api-server-down":
		return "", errors.New("the API server did not answer")
	case !known:
		return "", tokenreview.ErrNotAuthenticated
	default:
		return name, nil
	}
}

type actsHarness struct {
	t      *testing.T
	server *httptest.Server
	audit  *bytes.Buffer
	read   func(namespace, name string) (*gentianov1alpha1.TenantExport, error)
}

func startActs(t *testing.T, director string) *actsHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	elsewhere := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "moved"}}
	elsewhere.Spec.Isolation = &gentianov1alpha1.TenantIsolation{Namespace: "somewhere-else"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}, elsewhere).Build()
	out := &bytes.Buffer{}
	acts := NewActs(ClusterExports{Client: c}, tokens{"director": directorName, "usher": "system:serviceaccount:kernel-control:gentian-os-usher"},
		director, slog.New(slog.NewJSONHandler(out, nil)))
	server := httptest.NewServer(acts.Handler())
	t.Cleanup(server.Close)
	return &actsHarness{t: t, server: server, audit: out, read: func(namespace, name string) (*gentianov1alpha1.TenantExport, error) {
		export := &gentianov1alpha1.TenantExport{}
		return export, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, export)
	}}
}

func (h *actsHarness) post(token, path, body string) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set(ActorHeader, "ada")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(answer)
}

// The listener is the director's: a request without a token, with a token
// nobody vouches for, or with another program's is refused, and so is every
// request while the API server cannot be asked or no director is configured.
func TestTheActsListenerAdmitsTheDirectorAlone(t *testing.T) {
	h := startActs(t, directorName)
	for token, want := range map[string]int{
		"":                http.StatusUnauthorized,
		"made-up":         http.StatusUnauthorized,
		"usher":           http.StatusForbidden,
		"api-server-down": http.StatusServiceUnavailable,
		"director":        http.StatusAccepted,
	} {
		if status, body := h.post(token, "/v1/tenants/acme/actions/backup", `{"name":"by-`+strings.ReplaceAll(token, "-", "")+`x"}`); status != want {
			t.Errorf("token %q: status %d, want %d (%s)", token, status, want, body)
		}
	}
	if status, _ := startActs(t, "").post("director", "/v1/tenants/acme/actions/backup", `{}`); status != http.StatusServiceUnavailable {
		t.Errorf("a listener with no director configured answered %d", status)
	}
}

// A backup is the record the operator takes it from, in the tenant's own
// namespace, with who asked on it; deleting one removes the record.
func TestABackupIsARecordInTheTenantsNamespace(t *testing.T) {
	h := startActs(t, directorName)
	status, body := h.post("director", "/v1/tenants/acme/actions/backup", `{"name":"before-upgrade","apps":["wiki"],"recipients":["age1example"]}`)
	if status != http.StatusAccepted || !strings.Contains(body, `"name":"before-upgrade"`) || !strings.Contains(body, `"status":"started"`) {
		t.Fatalf("status %d, body %s", status, body)
	}
	export, err := h.read("tenant-acme", "before-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if export.Annotations[gentianov1alpha1.RequestedByAnnotation] != "ada" || len(export.Spec.Apps) != 1 ||
		export.Spec.Encryption == nil || export.Spec.Encryption.Recipients[0] != "age1example" {
		t.Fatalf("export = %+v", export)
	}
	if !strings.Contains(h.audit.String(), `"action":"backup"`) || !strings.Contains(h.audit.String(), `"subject":"ada"`) {
		t.Fatalf("no record of the act: %s", h.audit.String())
	}

	// A name is given when none is asked for, and a tenant kept elsewhere
	// has its record there.
	if status, body := h.post("director", "/v1/tenants/moved/actions/backup", ``); status != http.StatusAccepted || !strings.Contains(body, `"name":"manual-`) {
		t.Fatalf("status %d, body %s", status, body)
	}
	if status, _ := h.post("director", "/v1/tenants/moved/actions/backup", `{"name":"kept"}`); status != http.StatusAccepted {
		t.Fatalf("status %d", status)
	}
	if _, err := h.read("somewhere-else", "kept"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"the same name again", "/v1/tenants/acme/actions/backup", `{"name":"before-upgrade"}`, http.StatusBadRequest},
		{"a name that is not one", "/v1/tenants/acme/actions/backup", `{"name":"../x"}`, http.StatusBadRequest},
		{"a tenant that is not", "/v1/tenants/nobody/actions/backup", `{}`, http.StatusNotFound},
		{"deleting without a name", "/v1/tenants/acme/actions/delete-backup", `{}`, http.StatusBadRequest},
		{"deleting what is not", "/v1/tenants/acme/actions/delete-backup", `{"name":"never"}`, http.StatusNotFound},
		{"deleting another tenant's", "/v1/tenants/moved/actions/delete-backup", `{"name":"before-upgrade"}`, http.StatusNotFound},
		{"deleting the one made", "/v1/tenants/acme/actions/delete-backup", `{"name":"before-upgrade"}`, http.StatusAccepted},
	} {
		if status, body := h.post("director", tc.path, tc.body); status != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, status, tc.want, body)
		}
	}
}
