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
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// liveTenants answers the operator's GET /v1/tenants/{t} from a table the
// test changes, the way Argo CD syncing a commit changes the live Tenant.
type liveTenants struct {
	mu     sync.Mutex
	policy map[string]string
	asked  int
}

func (l *liveTenants) timesAsked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.asked
}

func (l *liveTenants) set(tenant, policy string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.policy[tenant] = policy
}

func (l *liveTenants) Get(_ context.Context, path string, _ url.Values) (int, []byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked++
	name := strings.TrimPrefix(path, "/v1/tenants/")
	p, ok := l.policy[name]
	if !ok {
		return http.StatusOK, []byte(`{"name":"` + name + `","exists":false}`), nil
	}
	return http.StatusOK, []byte(`{"name":"` + name + `","exists":true,"deletionPolicy":"` + p + `"}`), nil
}

func (l *liveTenants) Plans(context.Context, string, bool) ([]lifecycle.Plan, error) { return nil, nil }

func (l *liveTenants) Stream(context.Context, string) (*http.Response, error) {
	return nil, errors.New("no streams here")
}

func (l *liveTenants) Upload(context.Context, string, string, io.Reader) (int, []byte, error) {
	return http.StatusNotFound, nil, nil
}

func (l *liveTenants) Do(context.Context, string, string, any) (int, []byte, error) {
	return http.StatusNotFound, nil, nil
}

// A purge removes the tenant's manifest only after the live Tenant says
// Delete. Removed while the cluster still held Retain, the operator would tear
// it down keeping every database the purge was asked to delete.
func TestAPurgeWaitsForTheClusterBeforeRemovingTheTenant(t *testing.T) {
	defer api.SetPurgePoll(20 * time.Millisecond)()
	live := &liveTenants{policy: map[string]string{"demo": "Retain"}}
	h := startWith(t, live)
	alice := h.token(t, "gentian", "alice")
	manifest := "clusters/" + dt.Cluster + "/tenants/demo/tenant.yaml"

	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/demo/actions/purge", alice, "")
	if code != http.StatusAccepted || body["status"] != "purge_requested" {
		t.Fatalf("purge = %d %v, want 202 purge_requested", code, body)
	}
	if !strings.Contains(dt.RemoteFile(t, h.remote, manifest), "deletionPolicy: Delete") {
		t.Fatal("the first commit does not say Delete")
	}

	// The cluster has not synced it yet: the manifest stays.
	time.Sleep(150 * time.Millisecond)
	if _, err := tryRemoteFile(h, manifest); err != nil {
		t.Fatal("the tenant was removed while the cluster still held Retain")
	}

	live.set("demo", "Delete")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := tryRemoteFile(h, manifest); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tenant was not removed after the cluster took in Delete")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// What waits beside the requests does not outlive the server. Close ends the
// wait of a purge the cluster has not taken in and returns when it has ended:
// nothing asks the operator or touches the checkout afterwards. The purge is
// not lost by it -- the manifest still says Delete, which is where the next
// start finds it.
func TestClosingTheServerEndsAPurgeThatIsStillWaiting(t *testing.T) {
	defer api.SetPurgePoll(10 * time.Millisecond)()
	live := &liveTenants{policy: map[string]string{"demo": "Retain"}}
	srv, h := secondDirector(t, startWith(t, nil), live)
	alice := h.token(t, "gentian", "alice")
	manifest := "clusters/" + dt.Cluster + "/tenants/demo/tenant.yaml"

	if code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/demo/actions/purge", alice, ""); code != http.StatusAccepted {
		t.Fatalf("purge = %d %v, want 202", code, body)
	}
	for deadline := time.Now().Add(5 * time.Second); live.timesAsked() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the purge never asked the cluster")
		}
	}

	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the watcher is still waiting")
	}
	asked := live.timesAsked()
	time.Sleep(50 * time.Millisecond)
	if again := live.timesAsked(); again != asked {
		t.Fatalf("the cluster was asked %d more times after Close returned", again-asked)
	}
	if !strings.Contains(dt.RemoteFile(t, h.remote, manifest), "deletionPolicy: Delete") {
		t.Fatal("the request for the purge is no longer in git")
	}
}

func TestThePlatformTenantIsNotPurged(t *testing.T) {
	h := startWith(t, &liveTenants{policy: map[string]string{}})
	code, _ := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/platform/actions/purge", h.token(t, "gentian", "alice"), "")
	if code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("purge platform = %d, want a refusal", code)
	}
}

func tryRemoteFile(h *harness, path string) (string, error) {
	out, err := gitShow(h.remote, path)
	return out, err
}

func gitShow(remote, path string) (string, error) {
	out, err := exec.Command("git", "--git-dir", remote, "show", "main:"+path).Output()
	return string(out), err
}
