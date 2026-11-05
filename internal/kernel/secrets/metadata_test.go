/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Metadata reads <mount>/metadata/<path>: when the record was made and its
// current version, never its values.
func TestMetadataReadsTheRecordsMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/secret/metadata/gentian-os/kernel/a":
			_, _ = w.Write([]byte(`{"data":{"created_time":"2026-11-05T12:00:00.1Z","current_version":3,"versions":{}}}`))
		case "/v1/secret/metadata/gentian-os/kernel/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewKVClient(srv.URL, "role", "")
	c.SetStaticToken("test-token")
	s := NewSeeder(c, nil)
	ctx := context.Background()

	m, err := s.Metadata(ctx, "gentian-os/kernel/a")
	if err != nil || m != (RecordMetadata{CreatedTime: "2026-11-05T12:00:00.1Z", CurrentVersion: 3}) {
		t.Fatalf("metadata = %+v, %v", m, err)
	}
	if _, err := s.Metadata(ctx, "gentian-os/kernel/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing record: %v, want ErrNotFound", err)
	}
	if _, err := s.Metadata(ctx, "gentian-os/kernel/broken"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a failing vault: %v, want an error that is not ErrNotFound", err)
	}
}
