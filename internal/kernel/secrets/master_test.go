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
	"errors"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// The operator's Seeder holds no Deriver and reads the master password and
// salt from the vault. What it derives is exactly what a Seeder built with
// the same master password and salt derives: the golden values.
func TestTheMasterReadFromTheVaultGivesTheGoldenValues(t *testing.T) {
	ctx := context.Background()
	srv := newFakeBao()
	defer srv.Close()
	kv := newClient(t, srv.URL)
	if err := kv.Put(ctx, secrets.MasterPasswordPath, map[string]string{"value": goldenMaster, "salt": goldenSalt}); err != nil {
		t.Fatal(err)
	}
	got := seedAll(t, secrets.NewSeeder(kv, nil).WithMode(fixed(secrets.ModeDerived)))
	for k, want := range goldenDerived {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	key, err := secrets.NewSeeder(kv, nil).SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY")
	if err != nil || key != goldenDerivedKey {
		t.Errorf("derived key = %q, %v; want %q", key, err, goldenDerivedKey)
	}
}

// In derived mode, while the master password cannot be read, nothing is made
// and nothing is written; once it can be, the derived value is made, on the
// same Seeder, without a restart.
func TestDerivedModeWaitsForTheMasterPassword(t *testing.T) {
	ctx := context.Background()
	srv := newFakeBao()
	defer srv.Close()
	kv := newClient(t, srv.URL)
	s := secrets.NewSeeder(kv, nil).WithMode(fixed(secrets.ModeDerived))
	path := secrets.CategoryPath("acme", "wiki", "database")
	conn := secrets.DatabaseCreds{Host: "pg", Port: "5432", Name: "acme_wiki", User: "acme_wiki"}

	if _, err := s.SeedDatabase(ctx, "acme", "wiki", conn); !errors.Is(err, secrets.ErrNoMasterPassword) {
		t.Fatalf("seeding without a master password: %v; want ErrNoMasterPassword", err)
	}
	if _, err := s.SeedDerivedKey(ctx, "acme", "chat", "WEBUI_SECRET_KEY"); !errors.Is(err, secrets.ErrNoMasterPassword) {
		t.Fatalf("a derived key without a master password: %v; want ErrNoMasterPassword", err)
	}
	if _, err := kv.Get(ctx, path); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a value was written without the master password: %v", err)
	}

	if err := kv.Put(ctx, secrets.MasterPasswordPath, map[string]string{"value": goldenMaster, "salt": goldenSalt}); err != nil {
		t.Fatal(err)
	}
	db, err := s.SeedDatabase(ctx, "acme", "wiki", conn)
	if err != nil {
		t.Fatal(err)
	}
	if db.Password != goldenDerived["database password"] {
		t.Errorf("password = %q, want %q", db.Password, goldenDerived["database password"])
	}
}

// A master password without a salt, or a salt without a master password, is
// not derived from: the values would not be the cluster's.
func TestAnIncompleteMasterRecordIsRefused(t *testing.T) {
	ctx := context.Background()
	for name, rec := range map[string]map[string]string{
		"no salt":            {"value": goldenMaster},
		"empty salt":         {"value": goldenMaster, "salt": ""},
		"no master password": {"salt": goldenSalt},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := secrets.DeriverFromRecord(rec); err == nil {
				t.Error("DeriverFromRecord accepted the record")
			}
			srv := newFakeBao()
			defer srv.Close()
			kv := newClient(t, srv.URL)
			if err := kv.Put(ctx, secrets.MasterPasswordPath, rec); err != nil {
				t.Fatal(err)
			}
			_, err := secrets.NewSeeder(kv, nil).SeedAppSecret(ctx, "acme", "wiki", "session_key")
			if !errors.Is(err, secrets.ErrNoMasterPassword) {
				t.Errorf("seeding: %v; want ErrNoMasterPassword", err)
			}
			if _, err := kv.Get(ctx, secrets.InternalPath("acme", "wiki", "session_key")); !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("a value was written: %v", err)
			}
		})
	}
}

// Random mode never reads the master password, and makes values without one.
func TestRandomModeNeedsNoMasterPassword(t *testing.T) {
	ctx := context.Background()
	srv := newFakeBao()
	defer srv.Close()
	s := secrets.NewSeeder(newClient(t, srv.URL), nil).WithMode(fixed(secrets.ModeRandom))
	v, err := s.SeedAppSecret(ctx, "acme", "wiki", "session_key")
	if err != nil || len(v) != 40 {
		t.Fatalf("random app secret = %q, %v", v, err)
	}
	again, err := s.SeedAppSecret(ctx, "acme", "wiki", "session_key")
	if err != nil || again != v {
		t.Errorf("second pass = %q, %v; want %q", again, err, v)
	}
}
