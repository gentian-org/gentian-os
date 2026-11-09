/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package membership_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/membership"
)

// keycloak is People with accounts per realm. hidden accounts exist but are
// left out of the list, as a list read while accounts are added can skip one.
type keycloak struct {
	accounts map[string][]string
	hidden   map[string]bool
	down     map[string]bool
}

func (k *keycloak) AccountIDs(_ context.Context, realm string) ([]string, error) {
	if k.down[realm] {
		return nil, errors.New("status 503")
	}
	var out []string
	for _, id := range k.accounts[realm] {
		if !k.hidden[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (k *keycloak) Exists(_ context.Context, realm, id string) (bool, error) {
	for _, a := range k.accounts[realm] {
		if a == id {
			return true, nil
		}
	}
	return false, nil
}

func startSweepable(t *testing.T, store membership.Store) (*listener, *membership.Receiver, *recordingMirror) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	proj, err := membership.NewProjector(store, scope, quiet)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := membership.NewReceiver(map[string]ed25519.PublicKey{"k1": pub}, proj, quiet)
	if err != nil {
		t.Fatal(err)
	}
	mirror := &recordingMirror{}
	rc.SetMirror(mirror)
	ts := httptest.NewServer(rc)
	t.Cleanup(ts.Close)
	return &listener{t: t, url: ts.URL, keyID: "k1", key: priv}, rc, mirror
}

// A person removed from Keycloak whose deletion statement never arrived loses
// their memberships at the next sweep; everyone else keeps theirs.
func TestASweepRemovesThoseNoLongerInKeycloak(t *testing.T) {
	store := newStore(t)
	l, rc, mirror := startSweepable(t, store)
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-gone",
		Groups: []string{"gentian:tenant:demo:admins", "gentian:tenant:demo:members"}})
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-stay",
		Groups: []string{"gentian:tenant:demo:members"}})
	l.send(membership.Event{Realm: "kernel", Type: membership.TypeUserMemberships, User: "u-op",
		Groups: []string{"gentian:platform:operators", "gentian:tenant:platform:admins"}})
	mirror.calls = nil

	kc := &keycloak{accounts: map[string][]string{"demo": {"u-stay"}, "kernel": {"u-op"}}}
	departed, err := rc.Sweep(context.Background(), kc)
	if err != nil {
		t.Fatal(err)
	}
	if len(departed) != 1 || departed[0].Realm != "demo" || strings.Join(departed[0].People, " ") != "u-gone" || departed[0].Removed != 2 {
		t.Fatalf("departed = %+v", departed)
	}
	if got := groupsOf(t, store, "u-gone"); got != "" {
		t.Fatalf("u-gone still holds %q", got)
	}
	if got := groupsOf(t, store, "u-stay"); got != "gentian/tenant/demo/members" {
		t.Fatalf("u-stay holds %q", got)
	}
	if got := groupsOf(t, store, "u-op"); got != "gentian/platform/operators gentian/tenant/platform/admins" {
		t.Fatalf("u-op holds %q", got)
	}
	if strings.Join(mirror.calls, "\n") != "set demo u-gone []" {
		t.Fatalf("the app stores were told:\n%s", strings.Join(mirror.calls, "\n"))
	}

	// A statement about u-gone written before the sweep and delivered after it
	// does not give the memberships back.
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-gone",
		Time: time.Now().Add(-time.Minute).UnixMilli(), Groups: []string{"gentian:tenant:demo:admins"}})
	if got := groupsOf(t, store, "u-gone"); got != "" {
		t.Fatalf("an older statement gave back %q", got)
	}
}

// The list only nominates: a person it leaves out who Keycloak still has, by
// id, keeps everything.
func TestASweepAsksForEachPersonBeforeRemovingAnything(t *testing.T) {
	store := newStore(t)
	l, rc, _ := startSweepable(t, store)
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-skipped",
		Groups: []string{"gentian:tenant:demo:admins"}})

	kc := &keycloak{accounts: map[string][]string{"demo": {"u-skipped"}}, hidden: map[string]bool{"u-skipped": true}}
	departed, err := rc.Sweep(context.Background(), kc)
	if err != nil || len(departed) != 0 {
		t.Fatalf("departed = %+v, err = %v", departed, err)
	}
	if got := groupsOf(t, store, "u-skipped"); got != "gentian/tenant/demo/admins" {
		t.Fatalf("u-skipped holds %q", got)
	}
}

// A realm Keycloak does not answer for keeps everything and is named; the
// others are swept.
func TestARealmKeycloakDoesNotAnswerForIsLeftAsItIs(t *testing.T) {
	store := newStore(t)
	l, rc, _ := startSweepable(t, store)
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-a",
		Groups: []string{"gentian:tenant:demo:members"}})
	l.send(membership.Event{Realm: "other", Type: membership.TypeUserMemberships, User: "u-b",
		Groups: []string{"gentian:tenant:other:members"}})

	kc := &keycloak{accounts: map[string][]string{}, down: map[string]bool{"demo": true}}
	departed, err := rc.Sweep(context.Background(), kc)
	if err == nil || !strings.Contains(err.Error(), "realm demo") {
		t.Fatalf("err = %v", err)
	}
	if got := groupsOf(t, store, "u-a"); got != "gentian/tenant/demo/members" {
		t.Fatalf("u-a holds %q", got)
	}
	if len(departed) != 1 || departed[0].Realm != "other" {
		t.Fatalf("departed = %+v", departed)
	}
	if got := groupsOf(t, store, "u-b"); got != "" {
		t.Fatalf("u-b still holds %q", got)
	}
}
