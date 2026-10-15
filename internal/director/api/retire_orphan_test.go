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
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// orphanOperator stands in for the operator's remove-orphan action: what
// it answers, and what it was asked.
type orphanOperator struct {
	mu     sync.Mutex
	status int
	body   string
	asked  []string
}

func (o *orphanOperator) Get(context.Context, string, url.Values) (int, []byte, error) {
	return http.StatusNotFound, []byte(`{"detail":"no such read"}`), nil
}

func (o *orphanOperator) Stream(context.Context, string) (*http.Response, error) {
	return nil, io.EOF
}

func (o *orphanOperator) Upload(context.Context, string, string, io.Reader) (int, []byte, error) {
	return http.StatusNotFound, nil, nil
}

func (o *orphanOperator) Plans(context.Context, string, bool) ([]lifecycle.Plan, error) {
	return nil, nil
}

func (o *orphanOperator) Do(_ context.Context, path, actor string, _ any) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.asked = append(o.asked, actor+" "+path)
	return o.status, []byte(o.body), nil
}

// A retire of a tenant git does not describe asks the cluster to remove it
// as an orphan: what the operator answers decides between "removed", "not
// found" and a refusal in the operator's words. A tenant git does describe
// is retired in git and the cluster is not asked.
func TestRetiringATenantGitLacksRemovesTheOrphanTheClusterStillRuns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantCode int
		wantIn   string
	}{
		{"the cluster still ran it", http.StatusAccepted, `{"name":"ghost","application":"tenant-ghost"}`, http.StatusAccepted, "its Application tenant-ghost had been pruned"},
		{"already on its way out", http.StatusAccepted, `{"name":"ghost","application":"tenant-ghost","alreadyDeleting":true}`, http.StatusAccepted, "already deleting it"},
		{"the cluster does not hold it either", http.StatusNotFound, `{"detail":"tenants.gentianos.io \"ghost\" not found"}`, http.StatusNotFound, "tenant not found"},
		{"its Application exists", http.StatusConflict, `{"detail":"the tenant's Application still exists: Application tenant-ghost owns ghost; retire it through git","reason":"still-declared"}`, http.StatusConflict, "refused to remove it: the tenant's Application still exists"},
	} {
		op := &orphanOperator{status: tc.status, body: tc.body}
		h := startWith(t, op)
		alice := h.token(t, "gentian", "alice")
		code, body := h.do(t, "DELETE", "/v1/clusters/"+dt.Cluster+"/tenants/ghost", alice, "")
		if code != tc.wantCode {
			t.Fatalf("%s: %d %v", tc.name, code, body)
		}
		text, _ := body["detail"].(string)
		if text == "" {
			text, _ = body["error"].(string)
		}
		if !strings.Contains(text, tc.wantIn) {
			t.Errorf("%s: answer %q does not say %q", tc.name, text, tc.wantIn)
		}
		// Asked once, as the person retiring, for exactly this tenant.
		if len(op.asked) != 1 || !strings.HasPrefix(op.asked[0], "alice@") ||
			!strings.HasSuffix(op.asked[0], " /v1/tenants/ghost/actions/remove-orphan") {
			t.Errorf("%s: the operator was asked %v", tc.name, op.asked)
		}
	}
}
