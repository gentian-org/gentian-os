/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sendWhole sends a POST the way an HTTP/1 client that is not Go's does --
// the whole body first, then the answer is read -- and returns the answer,
// or the error the sender saw.
func sendWhole(t *testing.T, base, path string, body []byte) (*http.Response, []byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: operator\r\nContent-Type: application/x-tar\r\nContent-Length: %d\r\n\r\n", path, len(body))
	if _, err := io.WriteString(conn, head); err != nil {
		return nil, nil, err
	}
	for rest := body; len(rest) > 0; {
		n := min(len(rest), 64<<10)
		if _, err := conn.Write(rest[:n]); err != nil {
			return nil, nil, fmt.Errorf("sending the body: %w", err)
		}
		rest = rest[n:]
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the answer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	return resp, answer, err
}

// An upload refused part-way reaches its sender as the refusal -- the
// status and the reason -- and not as a reset of a connection it is still
// sending on; the reason is recorded on the upload as well.
func TestAnUploadRefusedPartWayReachesTheSender(t *testing.T) {
	ctx := context.Background()
	_, secret := clusterStorage(t)
	s, _ := sourcesService(t, secret)
	h := &HTTPServer{Service: s}
	srv := httptest.NewServer(http.HandlerFunc(h.handleBundleUpload))
	t.Cleanup(srv.Close)

	ref, err := s.StartUpload(ctx, "corp")
	if err != nil {
		t.Fatal(err)
	}
	// Not a tar: refused at its first header, with 32 MiB still to come.
	body := bytes.Repeat([]byte("x"), 32<<20)
	resp, answer, err := sendWhole(t, srv.URL, "/v1/bundles?tenant=corp&into="+ref.Prefix, body)
	if err != nil {
		t.Fatalf("the sender saw %v, not the refusal", err)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(answer), "not a bundle") {
		t.Fatalf("answer = %d %s", resp.StatusCode, answer)
	}

	state, err := s.UploadedFiles(ctx, "corp", ref.Prefix)
	if err != nil || !strings.Contains(state.Refused, "not a bundle") || state.Complete {
		t.Errorf("the upload's state is %+v, %v", state, err)
	}
}
