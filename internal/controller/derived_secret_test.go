/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const webuiKey = "WEBUI_SECRET_KEY"

func modeOf(mode secrets.Mode) secrets.ModeFunc {
	return func(context.Context) (secrets.Mode, error) { return mode, nil }
}

// A key a profile declares is a secret: on a derived cluster it follows from
// the master password, and it is not the digest of the tenant's and the
// app's name an earlier version delivered, which anybody who knew the two
// could compute.
func TestADeclaredKeyIsDerivedFromTheMasterPassword(t *testing.T) {
	w := newModelAccessWorld(t, []string{"chat"})
	w.pass(t)
	got := secretValue(w.secret(t, "chat"), webuiKey)
	if len(got) != 64 || got == namesOnlyDerivedKey("demo", "chat") {
		t.Fatalf("%s = %q", webuiKey, got)
	}

	other := newModelAccessWorld(t, []string{"chat"})
	other.r.Seeder = secrets.NewSeeder(other.vault, secrets.NewDeriver("another-master"))
	other.pass(t)
	if secretValue(other.secret(t, "chat"), webuiKey) == got {
		t.Error("a cluster with another master password has the same key")
	}

	same := newModelAccessWorld(t, []string{"chat"})
	same.pass(t)
	if secretValue(same.secret(t, "chat"), webuiKey) != got {
		t.Error("a cluster with the same master password has another key")
	}
}

// Each key a profile declares has a value of its own, and so has each app.
func TestDeclaredKeysDifferPerAppAndKey(t *testing.T) {
	w := newModelAccessWorld(t, []string{"chat", "talk"})
	w.profiles["chat"] = llmProfile("chat", webuiKey, "SECOND_KEY")
	w.profiles["talk"] = llmProfile("talk", webuiKey)
	w.pass(t)
	chat, talk := w.secret(t, "chat"), w.secret(t, "talk")
	if chat == nil || talk == nil {
		t.Fatal("a declaring app has no Secret")
	}
	a, b, c := secretValue(chat, webuiKey), secretValue(chat, "SECOND_KEY"), secretValue(talk, webuiKey)
	if a == "" || b == "" || c == "" || a == b || a == c || b == c {
		t.Errorf("values are not three of their own: %q %q %q", a, b, c)
	}
}

// Random: drawn once, kept in the vault, and the same on a second pass and
// for an operator that starts again with nothing but the vault.
func TestARandomDeclaredKeyIsStableOverPassesAndARestart(t *testing.T) {
	w := newModelAccessWorld(t, []string{"chat"})
	w.r.Seeder = secrets.NewSeeder(w.vault, secrets.NewDeriver("unit-test-master")).WithMode(modeOf(secrets.ModeRandom))
	w.pass(t)
	first := secretValue(w.secret(t, "chat"), webuiKey)
	if len(first) != 64 {
		t.Fatalf("%s = %q", webuiKey, first)
	}
	if held := w.vault[secrets.DerivedKeyPath("demo", "chat", webuiKey)]["value"]; held != first {
		t.Fatalf("the vault holds %q, the Secret %q", held, first)
	}

	derived := newModelAccessWorld(t, []string{"chat"})
	derived.pass(t)
	if secretValue(derived.secret(t, "chat"), webuiKey) == first {
		t.Fatal("random mode gave the derived value")
	}

	w.pass(t)
	if got := secretValue(w.secret(t, "chat"), webuiKey); got != first {
		t.Errorf("the key changed on a second pass: %q, was %q", got, first)
	}
	// A restart: a new Seeder over the same vault, holding no master password.
	w.r.Seeder = secrets.NewSeeder(w.vault, nil).WithMode(modeOf(secrets.ModeRandom))
	w.pass(t)
	if got := secretValue(w.secret(t, "chat"), webuiKey); got != first {
		t.Errorf("the key changed over a restart: %q, was %q", got, first)
	}
}

// Derived mode with no master password: no value is made -- not a random
// one, not the names' digest -- the app waits and is given no Secret, and the
// next pass that has the master password makes it.
func TestWithoutTheMasterPasswordADeclaredKeyIsNotMade(t *testing.T) {
	w := newModelAccessWorld(t, []string{"chat"})
	w.r.Seeder = secrets.NewSeeder(w.vault, nil)
	state := w.pass(t)
	if len(state.served) != 0 || len(state.waiting) != 1 || !strings.Contains(state.waiting[0], "master password") {
		t.Fatalf("state = %+v, want chat waiting for the master password", state)
	}
	if w.secret(t, "chat") != nil {
		t.Error("the app was given a Secret without its declared key")
	}
	if _, stored := w.vault[secrets.DerivedKeyPath("demo", "chat", webuiKey)]; stored {
		t.Error("a value was stored")
	}

	w.r.Seeder = secrets.NewSeeder(w.vault, secrets.NewDeriver("unit-test-master"))
	if state := w.pass(t); len(state.served) != 1 || len(state.waiting) != 0 {
		t.Fatalf("state = %+v, want chat served once the master password is there", state)
	}
	if got := secretValue(w.secret(t, "chat"), webuiKey); len(got) != 64 {
		t.Errorf("%s = %q", webuiKey, got)
	}
}

// The value an earlier version delivered is replaced by the first pass.
func TestTheNamesOnlyKeyIsReplaced(t *testing.T) {
	old := operatorModelSecret("demo", "chat", "sk-old")
	old.Data[webuiKey] = []byte(namesOnlyDerivedKey("demo", "chat"))
	w := newModelAccessWorld(t, []string{"chat"}, old)
	w.pass(t)
	if got := secretValue(w.secret(t, "chat"), webuiKey); got == namesOnlyDerivedKey("demo", "chat") || len(got) != 64 {
		t.Errorf("%s = %q", webuiKey, got)
	}

	// Without a vault as well: random, kept in the Secret, and then stable.
	old = operatorModelSecret("demo", "chat", "sk-old")
	old.Data[webuiKey] = []byte(namesOnlyDerivedKey("demo", "chat"))
	n := newModelAccessWorld(t, []string{"chat"}, old)
	n.r.Seeder = nil
	n.pass(t)
	first := secretValue(n.secret(t, "chat"), webuiKey)
	if first == namesOnlyDerivedKey("demo", "chat") || len(first) != 64 {
		t.Fatalf("%s = %q", webuiKey, first)
	}
	n.pass(t)
	if got := secretValue(n.secret(t, "chat"), webuiKey); got != first {
		t.Error("the key changed on a second pass without a vault")
	}
}
