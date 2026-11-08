/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func line(t *testing.T, r Record) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	Write(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)), r)
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON line: %q: %v", buf.String(), err)
	}
	return got
}

func TestTheEnvelopeIsWhatTheDesignNames(t *testing.T) {
	got := line(t, Record{
		Source: "bouncer", Kind: KindDecision, RequestID: "req-1",
		Subject: "2f2a266e-0000", Object: "app:acme/nextcloud", Action: "can_use",
		Outcome: Refused, Detail: map[string]any{"reason": "no-session", "cached": false},
	})
	want := map[string]any{
		"msg": "audit", "level": "INFO", "audit": true, "source": "bouncer", "kind": "decision",
		"request_id": "req-1", "tenant": "acme", "subject": "2f2a266e-0000",
		"object": "app:acme/nextcloud", "action": "can_use", "outcome": "refused",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["time"]; !ok {
		t.Error("no time")
	}
	detail, _ := got["detail"].(map[string]any)
	if detail["reason"] != "no-session" || detail["cached"] != false {
		t.Errorf("detail = %v", got["detail"])
	}
}

func TestEmptyFieldsAreLeftOutNotWrittenEmpty(t *testing.T) {
	got := line(t, Record{Source: "director", Kind: KindChange, Outcome: OK})
	for _, k := range []string{"request_id", "tenant", "subject", "object", "action", "detail"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s written although empty", k)
		}
	}
}

func TestAnExplicitTenantWinsOverTheObjects(t *testing.T) {
	got := line(t, Record{Source: "registrar", Kind: KindIdentity, Outcome: OK, Tenant: "beta", Object: "app:acme/x"})
	if got["tenant"] != "beta" {
		t.Errorf("tenant = %v", got["tenant"])
	}
}

func TestTenantOf(t *testing.T) {
	for object, want := range map[string]string{
		"tenant:acme":         "acme",
		"app:acme/nextcloud":  "acme",
		"app:noslash":         "",
		"cluster:demo":        "",
		"":                    "",
		"group:gentian:x#mem": "",
	} {
		if got := TenantOf(object); got != want {
			t.Errorf("TenantOf(%q) = %q, want %q", object, got, want)
		}
	}
}

func TestANilLoggerWritesNothing(t *testing.T) {
	Write(context.Background(), nil, Record{Source: "x"})
}
