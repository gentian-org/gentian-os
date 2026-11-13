/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func rightsAt(t *testing.T, handler http.HandlerFunc) *PlatformRights {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewPlatformRights(srv.URL, "the-key", nil)
}

func says(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestTheQuestionCarriesTheKeyAndTheAnswerIsThePlatforms(t *testing.T) {
	yes := rightsAt(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer the-key" || string(body) != `{"app":"projects","person":"anna"}` {
			t.Errorf("asked %s with %q", body, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"allowed":true}`))
	})
	if allowed, err := yes.MayUse(context.Background(), "anna", "projects"); err != nil || !allowed {
		t.Fatalf("allowed %v, %v", allowed, err)
	}
	if allowed, err := rightsAt(t, says(200, `{"allowed":false}`)).MayUse(context.Background(), "a", "b"); err != nil || allowed {
		t.Fatalf("allowed %v, %v", allowed, err)
	}
}

func TestAnythingButAnAnswerIsUnavailable(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500, 503} {
		if allowed, err := rightsAt(t, says(status, `{"allowed":true}`)).MayUse(context.Background(), "a", "b"); err == nil || allowed {
			t.Errorf("%d: allowed %v, %v", status, allowed, err)
		}
	}
	down := NewPlatformRights("http://127.0.0.1:1/check", "the-key", nil)
	if allowed, err := down.MayUse(context.Background(), "a", "b"); err == nil || allowed {
		t.Fatalf("down: allowed %v, %v", allowed, err)
	}
}

func TestAnAnswerThatIsNotAPlainYesIsANo(t *testing.T) {
	for _, body := range []string{`{"allowed":"yes"}`, `{}`, `{"allowed":1}`} {
		if allowed, err := rightsAt(t, says(200, body)).MayUse(context.Background(), "a", "b"); err != nil || allowed {
			t.Errorf("%s: allowed %v, %v", body, allowed, err)
		}
	}
}
