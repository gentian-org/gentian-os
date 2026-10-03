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

package lifecycle

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The operator answers 401 only to a wrong shared token. That is a fault
// between the director and the operator, and it must not reach a browser as a
// 401, which a console reads as an expired session and answers by signing in
// again -- in a loop, several times a second.
func TestAnOperatorRefusingTheTokenIsABadGatewayNotAnExpiredSession(t *testing.T) {
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"this API is the director's; present its token"}`))
	}))
	defer op.Close()
	c := New(op.URL, "wrong")

	if status, body, err := c.Get(context.Background(), "/v1/tenants/demo/backups", nil); err != nil || status != http.StatusBadGateway || !strings.Contains(string(body), "refused the director's token") {
		t.Fatalf("Get = %d %s %v", status, body, err)
	}
	if status, _, err := c.Do(context.Background(), "/v1/tenants/demo/actions/backup", "ada", map[string]string{}); err != nil || status != http.StatusBadGateway {
		t.Fatalf("Do = %d %v", status, err)
	}
	if status, _, err := c.Upload(context.Background(), "/v1/bundles", "application/x-tar", strings.NewReader("x")); err != nil || status != http.StatusBadGateway {
		t.Fatalf("Upload = %d %v", status, err)
	}
	resp, err := c.Stream(context.Background(), "/v1/tenants/demo/backups/x/download")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Content-Disposition") != "" || !strings.Contains(string(body), "refused") {
		t.Fatalf("Stream = %d %q %s", resp.StatusCode, resp.Header.Get("Content-Disposition"), body)
	}
}
