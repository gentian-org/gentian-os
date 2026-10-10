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
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	goldenMaster = "golden-master-password"
	goldenSalt   = "0123456789abcdef0123456789abcdef"
)

// goldenDerived is what the master password and salt above give for one
// tenant's app, written down as text. It is the whole of what "derived" means
// to a cluster that already exists: these are the passwords its databases,
// buckets and clients were made with, so a change to any of them is a change
// that locks every app out of its own data.
var goldenDerived = map[string]string{
	"oidc client-secret":       "6daf68fa3f256e30d355ba0b8bb3974c4cf6c2cc",
	"database password":        "449e600429037c259e2bb185e6e0daf86c4bc8c4",
	"kernel database password": "f9df27d98b773e9283877e3c5532e070c478554c",
	"s3 access-key":            "8fb68a38cf4a0b02a89c",
	"s3 secret-key":            "86eec27dd04d486f2346d5770d25ce0c6920b286",
	"cache password":           "68835aa2be38d50506764a46c13fc56afb5d15ad",
	"llm api-key":              "sk-aa03a1a924848cefb82e3fe680481ac8bdc91f0d961d8a92",
	"app secret":               "e1fafd93b2b1873e520743b917d375ac994bf6d8",
	"contract password":        "24cf8325cb66f80dc64ae7491519ef482b35a187",
}

func fixed(mode secrets.Mode) secrets.ModeFunc {
	return func(context.Context) (secrets.Mode, error) { return mode, nil }
}

// seedAll makes every kind of value the Seeder generates, for one tenant's app.
func seedAll(t *testing.T, s *secrets.Seeder) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	oidc, err := s.SeedOIDC(ctx, "acme", "wiki", "https://id.example/realms/acme", "acme-wiki")
	if err != nil {
		t.Fatalf("oidc: %v", err)
	}
	out["oidc client-secret"] = oidc.ClientSecret
	db, err := s.SeedDatabase(ctx, "acme", "wiki", secrets.DatabaseCreds{Host: "pg", Port: "5432", Name: "acme_wiki", User: "acme_wiki"})
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	out["database password"] = db.Password
	kdb, err := s.SeedKernelDatabase(ctx, "shared", secrets.DatabaseCreds{Host: "pg", Port: "5432", Name: "shared", User: "shared"})
	if err != nil {
		t.Fatalf("kernel database: %v", err)
	}
	out["kernel database password"] = kdb.Password
	s3, err := s.SeedS3(ctx, "acme", "wiki", secrets.S3Creds{Endpoint: "http://minio:9000", Bucket: "acme-wiki", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("s3: %v", err)
	}
	out["s3 access-key"], out["s3 secret-key"] = s3.AccessKey, s3.SecretKey
	cache, err := s.SeedCache(ctx, "acme", "wiki", secrets.CacheCreds{Host: "redis", Port: "6379", User: "acme-wiki"})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	out["cache password"] = cache.Password
	llm, err := s.SeedModelAccess(ctx, "acme", "wiki", "sk-", "http://gateway:4000/v1")
	if err != nil {
		t.Fatalf("model access: %v", err)
	}
	out["llm api-key"] = llm.APIKey
	app, err := s.SeedAppSecret(ctx, "acme", "wiki", "session_key")
	if err != nil {
		t.Fatalf("app secret: %v", err)
	}
	out["app secret"] = app
	contract, err := s.SeedContract(ctx, "acme", "files")
	if err != nil {
		t.Fatalf("contract: %v", err)
	}
	out["contract password"] = contract.Password
	return out
}

// Derived mode gives the values it always gave: with no mode stated, as every
// Seeder was built before the mode was read, and with the mode read as derived.
func TestDerivedModeGivesTheValuesItAlwaysGave(t *testing.T) {
	for name, mode := range map[string]secrets.ModeFunc{
		"no mode stated": nil,
		"mode derived":   fixed(secrets.ModeDerived),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeBao()
			defer srv.Close()
			s := secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver(goldenMaster, goldenSalt))
			if mode != nil {
				s = s.WithMode(mode)
			}
			got := seedAll(t, s)
			for k, want := range goldenDerived {
				if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
			if len(got) != len(goldenDerived) {
				t.Errorf("%d values made, %d written down", len(got), len(goldenDerived))
			}
		})
	}
}

// Random mode gives values the master password does not lead to, each its
// own, and the same ones on every later pass — also from another Seeder, which
// is what an operator restart is. An app's own secret is one of them: a
// bundle carries it with the data that was written with it.
func TestRandomModeIsIndependentOfTheMasterAndStable(t *testing.T) {
	srv := newFakeBao()
	defer srv.Close()
	random := func() *secrets.Seeder {
		return secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver(goldenMaster, goldenSalt)).
			WithMode(fixed(secrets.ModeRandom))
	}
	first := seedAll(t, random())
	seen := map[string]string{}
	for k, v := range first {
		if v == "" {
			t.Errorf("%s is empty", k)
		}
		if v == goldenDerived[k] {
			t.Errorf("%s = %q in random mode; the derived value is %q", k, v, goldenDerived[k])
		}
		if other, dup := seen[v]; dup {
			t.Errorf("%s and %s hold the same value", k, other)
		}
		seen[v] = k
	}
	if !strings.HasPrefix(first["llm api-key"], "sk-") || len(first["llm api-key"]) != len("sk-")+48 {
		t.Errorf("llm api-key = %q", first["llm api-key"])
	}
	for pass, s := range map[string]*secrets.Seeder{"second pass": random(), "after a restart": random()} {
		again := seedAll(t, s)
		for k, v := range first {
			if again[k] != v {
				t.Errorf("%s: %s changed from %q to %q", pass, k, v, again[k])
			}
		}
	}

	// Another cluster with the same master password holds other values.
	other := newFakeBao()
	defer other.Close()
	elsewhere := seedAll(t, secrets.NewSeeder(newClient(t, other.URL), secrets.NewDeriver(goldenMaster, goldenSalt)).
		WithMode(fixed(secrets.ModeRandom)))
	for k, v := range first {
		if elsewhere[k] == v {
			t.Errorf("%s on a second cluster: %q, here %q", k, elsewhere[k], v)
		}
	}
}

// A change of mode leaves what exists: values made as derived stay when the
// mode becomes random, values made as random stay when it becomes derived,
// and only what is made afterwards follows the new mode.
func TestChangingTheModeLeavesExistingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to secrets.Mode
	}{
		{"derived to random", secrets.ModeDerived, secrets.ModeRandom},
		{"random to derived", secrets.ModeRandom, secrets.ModeDerived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeBao()
			defer srv.Close()
			mode := tc.from
			s := secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver(goldenMaster, goldenSalt)).
				WithMode(func(context.Context) (secrets.Mode, error) { return mode, nil })
			before := seedAll(t, s)
			mode = tc.to
			after := seedAll(t, s)
			for k, v := range before {
				if after[k] != v {
					t.Errorf("%s changed from %q to %q", k, v, after[k])
				}
			}
			// What is made after the change follows the new mode.
			later, err := s.SeedContract(context.Background(), "acme", "calendar")
			if err != nil {
				t.Fatal(err)
			}
			derived := secrets.NewDeriver(goldenMaster, goldenSalt).
				Derive(secrets.ContractPath("acme", "calendar"), "password", 40)
			if (later.Password == derived) != (tc.to == secrets.ModeDerived) {
				t.Errorf("made after the change to %s: %q (derived would be %q)", tc.to, later.Password, derived)
			}
		})
	}
}

// A mode that cannot be read, or that is neither of the two, makes nothing:
// a guess would decide for good how a credential was made.
func TestUnknownModeMakesNothing(t *testing.T) {
	for name, mode := range map[string]secrets.ModeFunc{
		"unreadable": func(context.Context) (secrets.Mode, error) { return "", errors.New("api server away") },
		"misspelt":   fixed("Random"),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeBao()
			defer srv.Close()
			kv := newClient(t, srv.URL)
			s := secrets.NewSeeder(kv, secrets.NewDeriver(goldenMaster, goldenSalt)).WithMode(mode)
			ctx := context.Background()
			if _, err := s.SeedContract(ctx, "acme", "files"); err == nil {
				t.Error("a contract password was made")
			}
			if _, err := s.SeedDatabase(ctx, "acme", "wiki", secrets.DatabaseCreds{User: "u"}); err == nil {
				t.Error("a database password was made")
			}
			if _, err := s.SeedOIDC(ctx, "acme", "wiki", "https://id.example", "c"); err == nil {
				t.Error("a client secret was made")
			}
			for _, p := range []string{
				secrets.ContractPath("acme", "files"),
				secrets.CategoryPath("acme", "wiki", "database"),
				secrets.CategoryPath("acme", "wiki", "oidc"),
			} {
				if got, _ := kv.Get(ctx, p); got != nil {
					t.Errorf("%s was written: %v", p, got)
				}
			}
		})
	}
}

// deafStore keeps what is written and fails every read after the first
// failAfter of them, as a vault does that answers a write and then drops out.
type deafStore struct {
	mu        sync.Mutex
	data      map[string]map[string]string
	reads     int
	failAfter int
}

func (d *deafStore) PutOnce(_ context.Context, p string, data map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.data[p]; !ok {
		d.data[p] = data
	}
	return nil
}

func (d *deafStore) Put(_ context.Context, p string, data map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[p] = data
	return nil
}

func (d *deafStore) Get(_ context.Context, p string) (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	if d.reads > d.failAfter {
		return nil, errors.New("vault away")
	}
	v, ok := d.data[p]
	if !ok {
		return nil, errors.New("not found")
	}
	return v, nil
}

// A random value that could not be read back is not handed on: the path may
// hold an earlier one, and the caller would set a database role to a password
// nothing stores. A derived value is the same either way, and still is.
func TestRandomValueIsNotReturnedUnread(t *testing.T) {
	ctx := context.Background()
	stored := map[string]string{"host": "pg", "port": "5432", "name": "n", "user": "u", "password": "the-stored-one"}
	path := secrets.CategoryPath("acme", "wiki", "database")

	store := &deafStore{data: map[string]map[string]string{path: stored}}
	random := secrets.NewSeeder(store, secrets.NewDeriver(goldenMaster, goldenSalt)).WithMode(fixed(secrets.ModeRandom))
	if got, err := random.SeedDatabase(ctx, "acme", "wiki", secrets.DatabaseCreds{User: "u"}); err == nil {
		t.Errorf("random mode returned %q for a path it could not read", got.Password)
	}
	if got, err := random.SeedContract(ctx, "acme", "files"); err == nil {
		t.Errorf("random mode returned contract password %q unread", got.Password)
	}
	if got, err := random.SeedCache(ctx, "acme", "wiki", secrets.CacheCreds{Host: "r"}); err == nil {
		t.Errorf("random mode returned cache password %q unread", got.Password)
	}
	if got, err := random.SeedOIDC(ctx, "acme", "wiki", "https://id.example", "c"); err == nil {
		t.Errorf("random mode returned client secret %q unread", got.ClientSecret)
	}
	if store.data[path]["password"] != "the-stored-one" {
		t.Errorf("the stored password was replaced: %v", store.data[path])
	}

	derived := secrets.NewSeeder(&deafStore{data: map[string]map[string]string{}},
		secrets.NewDeriver(goldenMaster, goldenSalt)).WithMode(fixed(secrets.ModeDerived))
	got, err := derived.SeedDatabase(ctx, "acme", "wiki", secrets.DatabaseCreds{User: "u"})
	if err != nil || got.Password != goldenDerived["database password"] {
		t.Errorf("derived mode: %q, %v; want the derived password", got.Password, err)
	}
}

// An app's own secret follows the mode like every other generated value:
// derived, it is the value it always was; random, it is another on a vault
// emptied by a purge, which is why a bundle carries it.
func TestAppSecretFollowsTheMode(t *testing.T) {
	ctx := context.Background()
	make1 := func(mode secrets.Mode) string {
		srv := newFakeBao()
		defer srv.Close()
		s := secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver(goldenMaster, goldenSalt)).WithMode(fixed(mode))
		got, err := s.SeedAppSecret(ctx, "acme", "wiki", "session_key")
		if err != nil || len(got) != 40 {
			t.Fatalf("%s: app secret = %q, %v", mode, got, err)
		}
		return got
	}
	if got := make1(secrets.ModeDerived); got != goldenDerived["app secret"] {
		t.Errorf("derived: app secret = %q; want %q", got, goldenDerived["app secret"])
	}
	first, second := make1(secrets.ModeRandom), make1(secrets.ModeRandom)
	if first == goldenDerived["app secret"] || first == second {
		t.Errorf("random: app secret = %q, and %q on an emptied vault; the derived one is %q", first, second, goldenDerived["app secret"])
	}
}

// A restore sets an app's own secret to the value its data was written with:
// the one stored value that is replaced. Where the path holds that value
// already nothing is written; a path that cannot be read is an error and not
// "nothing stored", and so is a value the vault does not give back.
func TestReplaceAppSecretSetsTheValueAndSaysWhetherItChanged(t *testing.T) {
	ctx := context.Background()
	srv := newFakeBao()
	defer srv.Close()
	kv := newClient(t, srv.URL)
	s := secrets.NewSeeder(kv, secrets.NewDeriver(goldenMaster, goldenSalt)).WithMode(fixed(secrets.ModeRandom))

	if _, found, err := s.ReadAppSecret(ctx, "acme", "wiki", "session_key"); found || err != nil {
		t.Fatalf("an empty path read as found = %v, %v", found, err)
	}
	made, err := s.SeedAppSecret(ctx, "acme", "wiki", "session_key")
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.ReadAppSecret(ctx, "acme", "wiki", "session_key"); !found || err != nil || got != made {
		t.Fatalf("read back %q, %v, %v; made %q", got, found, err, made)
	}
	if changed, err := s.ReplaceAppSecret(ctx, "acme", "wiki", "session_key", made); changed || err != nil {
		t.Errorf("the same value: changed = %v, %v", changed, err)
	}
	if changed, err := s.ReplaceAppSecret(ctx, "acme", "wiki", "session_key", "the-bundles-value"); !changed || err != nil {
		t.Fatalf("another value: changed = %v, %v", changed, err)
	}
	// It stands: a later pass of the seeder keeps it.
	if got, err := s.SeedAppSecret(ctx, "acme", "wiki", "session_key"); err != nil || got != "the-bundles-value" {
		t.Errorf("after the replacement the seeder returns %q, %v", got, err)
	}
	stored, _ := kv.Get(ctx, secrets.InternalPath("acme", "wiki", "session_key"))
	if len(stored) != 1 || stored["value"] != "the-bundles-value" {
		t.Errorf("the path holds %v", stored)
	}
	if _, err := s.ReplaceAppSecret(ctx, "acme", "wiki", "session_key", ""); err == nil {
		t.Error("an empty value was written")
	}

	// A key declared under spec.secrets.derived is set the same way, at its
	// own path, and the generated secret of the same name is another thing.
	key, err := s.SeedDerivedKey(ctx, "acme", "wiki", "session_key")
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.ReadDerivedKey(ctx, "acme", "wiki", "session_key"); !found || err != nil || got != key {
		t.Fatalf("declared key read back %v, %v", found, err)
	}
	if changed, err := s.ReplaceDerivedKey(ctx, "acme", "wiki", "session_key", "the-bundles-key"); !changed || err != nil {
		t.Fatalf("declared key: changed = %v, %v", changed, err)
	}
	if got, err := s.SeedDerivedKey(ctx, "acme", "wiki", "session_key"); err != nil || got != "the-bundles-key" {
		t.Errorf("after the replacement the seeder returns %q, %v", got, err)
	}
	if got, _, _ := s.ReadAppSecret(ctx, "acme", "wiki", "session_key"); got != "the-bundles-value" {
		t.Errorf("replacing the declared key changed the generated secret to %q", got)
	}
	if _, found, err := s.ReadDerivedKey(ctx, "acme", "wiki", "never_made"); found || err != nil {
		t.Errorf("a key never made: found %v, %v", found, err)
	}

	away := secrets.NewSeeder(&deafStore{data: map[string]map[string]string{}}, nil)
	if _, _, err := away.ReadAppSecret(ctx, "acme", "wiki", "session_key"); err == nil {
		t.Error("a vault that does not answer read as an empty path")
	}
	if _, err := away.ReplaceAppSecret(ctx, "acme", "wiki", "session_key", "v"); err == nil {
		t.Error("a value was set in a vault that does not answer")
	}
}
