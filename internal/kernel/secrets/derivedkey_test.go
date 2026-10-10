/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// goldenDerivedKey is what the golden master password and salt give for the
// key WEBUI_SECRET_KEY of tenant acme's app chat. An app signs sessions with
// such a key and may encrypt what it stores with it, so on a derived cluster
// a change to this value is a change that ends every session and makes that
// data unreadable.
const goldenDerivedKey = "4b08b044bd086e09ca248667e6c41dd1a202efa927cc060d85048fa312e12e85"

func derivedKeySeeder(w secrets.Writer, d *secrets.Deriver, mode secrets.Mode) *secrets.Seeder {
	return secrets.NewSeeder(w, d).WithMode(fixed(mode))
}

func TestDerivedKeyInDerivedModeIsTheGoldenValue(t *testing.T) {
	ctx := context.Background()
	srv := newFakeBao()
	defer srv.Close()
	s := derivedKeySeeder(newClient(t, srv.URL), secrets.NewDeriver(goldenMaster, goldenSalt), secrets.ModeDerived)
	got, err := s.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenDerivedKey {
		t.Fatalf("derived key = %q, want %q", got, goldenDerivedKey)
	}
	// A cluster rebuilt from the same master password and salt arrives at it
	// again, with nothing stored.
	fresh := newFakeBao()
	defer fresh.Close()
	again, err := derivedKeySeeder(newClient(t, fresh.URL), secrets.NewDeriver(goldenMaster, goldenSalt), secrets.ModeDerived).
		SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
	if err != nil || again != got {
		t.Errorf("a second cluster made %q, %v; want %q", again, err, got)
	}
}

// The value is not what anybody who knows the two names can compute, and it
// follows from the master password and from the salt.
func TestDerivedKeyNeedsTheMasterPasswordAndTheSalt(t *testing.T) {
	ctx := context.Background()
	make1 := func(master, salt string) string {
		t.Helper()
		got, err := derivedKeySeeder(&deafStore{data: map[string]map[string]string{}, failAfter: 1 << 30},
			secrets.NewDeriver(master, salt), secrets.ModeDerived).SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := make1(goldenMaster, goldenSalt)
	if make1("another-master", goldenSalt) == base {
		t.Error("the value does not depend on the master password")
	}
	if make1(goldenMaster, "ffffffffffffffffffffffffffffffff") == base {
		t.Error("the value does not depend on the salt")
	}
	h := sha256.Sum256([]byte("acme-chat-secret-salt-value"))
	if base == base64.URLEncoding.EncodeToString(h[:]) {
		t.Error("the value is the one computed from the tenant's and the app's name alone")
	}
}

func TestDerivedKeyDiffersPerTenantAppAndKey(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []secrets.Mode{secrets.ModeDerived, secrets.ModeRandom} {
		s := derivedKeySeeder(&deafStore{data: map[string]map[string]string{}, failAfter: 1 << 30},
			secrets.NewDeriver(goldenMaster, goldenSalt), mode)
		seen := map[string]string{}
		for _, c := range [][3]string{
			{"acme", "chat", "WEBUI_SECRET_KEY"},
			{"globex", "chat", "WEBUI_SECRET_KEY"},
			{"acme", "notes", "WEBUI_SECRET_KEY"},
			{"acme", "chat", "OTHER_KEY"},
		} {
			got, err := s.SeedDerivedKey(ctx, c[0], c[1], c[2])
			if err != nil {
				t.Fatalf("%s %v: %v", mode, c, err)
			}
			if len(got) != 64 {
				t.Errorf("%s %v: %d characters, want 64", mode, c, len(got))
			}
			if other, dup := seen[got]; dup {
				t.Errorf("%s: %v and %s share a value", mode, c, other)
			}
			seen[got] = c[0] + "/" + c[1] + "/" + c[2]
		}
	}
}

// Random: drawn once, stored, and the same on every later pass -- also for a
// Seeder built anew, as after a restart of the operator, and whatever master
// password that Seeder holds.
func TestRandomDerivedKeyIsStoredOnceAndStable(t *testing.T) {
	ctx := context.Background()
	srv := newFakeBao()
	defer srv.Close()
	first := derivedKeySeeder(newClient(t, srv.URL), secrets.NewDeriver(goldenMaster, goldenSalt), secrets.ModeRandom)
	a, err := first.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if a == goldenDerivedKey {
		t.Fatal("random mode gave the derived value")
	}
	b, err := first.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
	if err != nil || b != a {
		t.Fatalf("second pass = %q, %v; want %q", b, err, a)
	}
	for _, d := range []*secrets.Deriver{nil, secrets.NewDeriver("another-master")} {
		restarted := derivedKeySeeder(newClient(t, srv.URL), d, secrets.ModeRandom)
		c, err := restarted.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
		if err != nil || c != a {
			t.Fatalf("after a restart = %q, %v; want %q", c, err, a)
		}
	}
	// And a change of mode leaves it.
	derived := derivedKeySeeder(newClient(t, srv.URL), secrets.NewDeriver(goldenMaster, goldenSalt), secrets.ModeDerived)
	if c, err := derived.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY"); err != nil || c != a {
		t.Fatalf("after a change of mode = %q, %v; want %q", c, err, a)
	}
	// Another cluster draws another.
	other := newFakeBao()
	defer other.Close()
	d, err := derivedKeySeeder(newClient(t, other.URL), nil, secrets.ModeRandom).SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
	if err != nil || d == a {
		t.Errorf("a second cluster drew %q, %v; want a value of its own", d, err)
	}
}

// Derived mode without the master password makes no value: not a random one,
// and not one anybody could compute. What is already stored is still served.
func TestDerivedKeyWithoutTheMasterPasswordIsNotMade(t *testing.T) {
	ctx := context.Background()
	for name, d := range map[string]*secrets.Deriver{"no deriver": nil, "empty master": secrets.NewDeriver("")} {
		store := &deafStore{data: map[string]map[string]string{}, failAfter: 1 << 30}
		s := derivedKeySeeder(store, d, secrets.ModeDerived)
		got, err := s.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
		if !errors.Is(err, secrets.ErrNoMasterPassword) || got != "" {
			t.Errorf("%s: got %q, %v; want no value and ErrNoMasterPassword", name, got, err)
		}
		if len(store.data) != 0 {
			t.Errorf("%s: something was stored: %v", name, store.data)
		}

		store.data[secrets.DerivedKeyPath("acme", "chat", "WEBUI_SECRET_KEY")] = map[string]string{"value": "stored-earlier"}
		got, err = s.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
		if err != nil || got != "stored-earlier" {
			t.Errorf("%s: got %q, %v; want the stored value", name, got, err)
		}
	}
}

// A mode that cannot be read, or that is neither of the two, makes nothing.
func TestDerivedKeyWithoutAKnownModeIsNotMade(t *testing.T) {
	ctx := context.Background()
	store := &deafStore{data: map[string]map[string]string{}, failAfter: 1 << 30}
	unknown := secrets.NewSeeder(store, secrets.NewDeriver(goldenMaster, goldenSalt)).WithMode(
		func(context.Context) (secrets.Mode, error) { return "", errors.New("no claim") })
	if got, err := unknown.SeedDerivedKey(ctx, "acme", "chat", "K"); err == nil || got != "" {
		t.Errorf("got %q, %v; want an error", got, err)
	}
	odd := derivedKeySeeder(store, secrets.NewDeriver(goldenMaster, goldenSalt), "sometimes")
	if got, err := odd.SeedDerivedKey(ctx, "acme", "chat", "K"); err == nil || got != "" {
		t.Errorf("got %q, %v; want an error", got, err)
	}
	if len(store.data) != 0 {
		t.Errorf("something was stored: %v", store.data)
	}
}

// A value that was written and could not be read back is not handed on.
func TestDerivedKeyIsNotReturnedUnread(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []secrets.Mode{secrets.ModeDerived, secrets.ModeRandom} {
		store := &deafStore{data: map[string]map[string]string{}}
		s := derivedKeySeeder(store, secrets.NewDeriver(goldenMaster, goldenSalt), mode)
		if got, err := s.SeedDerivedKey(ctx, "acme", "chat", "K"); err == nil {
			t.Errorf("%s: returned %q for a path it could not read", mode, got)
		}
	}
}
