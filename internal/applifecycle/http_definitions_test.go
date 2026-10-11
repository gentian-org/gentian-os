/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// The director learns what the cluster serves from this listener, with the
// ServiceAccount token it already presents here, and through the client it
// already uses -- so the two ends are tested against each other and not each
// against its own idea of the path and the body.
func TestTheDirectorReadsTheDefinitionsCheckWithItsOwnIdentity(t *testing.T) {
	set := schemacheck.Embedded()
	gate := schemacheck.NewGate(set)
	_, auth := cluster()
	h := &HTTPServer{Auth: auth, Definitions: gate}
	srv := httptest.NewServer(h.routes())
	defer srv.Close()
	as := func(token string) func() string { return func() string { return token } }

	// Before the operator has read the cluster: answered, and as not checked.
	got, err := lifecycle.New(srv.URL, as("director")).Definitions(context.Background())
	if err != nil {
		t.Fatalf("the director was not answered: %v", err)
	}
	if got.Checked || got.OK || got.Definitions != set.Digest {
		t.Fatalf("an operator that has not checked yet answered %+v", got)
	}

	published := schemacheck.Report{
		Checked: true, CheckedAt: "2026-10-07T10:00:00Z", Version: "abc123", Definitions: set.Digest,
		Kinds: []schemacheck.KindReport{
			{Kind: "Tenant", CRD: "tenants.gentianos.io", Source: schemacheck.SourceChart, State: schemacheck.StateOutdated,
				Missing: []string{"spec.apps[].addonPins"}, Remedy: schemacheck.RemedyChart},
			{Kind: "App", CRD: "apps.gentianos.io", Source: schemacheck.SourceXRD, XRD: "xapps.gentianos.io", State: schemacheck.StateOK},
		},
	}
	gate.Publish(published)
	got, err = lifecycle.New(srv.URL, as("director")).Definitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, published) {
		t.Fatalf("the director read\n%+v\nthe operator published\n%+v", got, published)
	}

	// The same answer under the usher's identity: an operations screen shows
	// it with the permission it already has.
	if got, err := lifecycle.NewReader(srv.URL, as("usher")).Definitions(context.Background()); err != nil || !reflect.DeepEqual(got, published) {
		t.Fatalf("the usher: %+v, %v", got, err)
	}

	// Nobody else, and never as an answer the director could take for one.
	for _, token := range []string{"custodian", "impostor", "director-for-the-api-server", ""} {
		_, err := lifecycle.New(srv.URL, as(token)).Definitions(context.Background())
		var upstream *lifecycle.UpstreamError
		if !errors.As(err, &upstream) || upstream.Status == http.StatusOK {
			t.Errorf("token %q: want a refusal, got %v", token, err)
		}
	}
	if got := answer(h.routes(), "POST", DefinitionsPath, "director"); got != http.StatusMethodNotAllowed {
		t.Errorf("the route is a read; POST answered %d", got)
	}
}

// An operator that runs no check says "not checked", which every caller that
// must not assume reads as it should.
func TestAnOperatorWithoutTheCheckAnswersNotChecked(t *testing.T) {
	_, auth := cluster()
	h := &HTTPServer{Auth: auth}
	srv := httptest.NewServer(h.routes())
	defer srv.Close()
	got, err := lifecycle.New(srv.URL, func() string { return "director" }).Definitions(context.Background())
	if err != nil || got.Checked || got.OK {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestTheTwoEndsAgreeOnThePath(t *testing.T) {
	if DefinitionsPath != lifecycle.DefinitionsPath {
		t.Fatalf("the operator serves %s and the director asks %s", DefinitionsPath, lifecycle.DefinitionsPath)
	}
}
