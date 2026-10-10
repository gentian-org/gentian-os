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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Writer is the narrow slice of KVClient that the Seeder needs. It exists so
// reconciler tests can substitute an in-memory fake.
type Writer interface {
	PutOnce(ctx context.Context, logicalPath string, data map[string]string) error
	Put(ctx context.Context, logicalPath string, data map[string]string) error
	Get(ctx context.Context, logicalPath string) (map[string]string, error)
}

// Mode is how the Seeder makes a value that nothing has stored yet. It is the
// cluster's secretMode, a field of the Cluster claim.
type Mode string

const (
	// ModeDerived computes the value from the master password, so a cluster
	// rebuilt from the same master password and salt arrives at it again.
	ModeDerived Mode = "derived"
	// ModeRandom draws the value from crypto/rand. Nothing reproduces it: the
	// stored copy is the only one.
	ModeRandom Mode = "random"
)

// ModeFunc reports the cluster's mode. It is asked each time a value is made,
// so a Seeder built before the claim could be read does not keep a guess. An
// error means the mode is not known, and then no value is made.
type ModeFunc func(ctx context.Context) (Mode, error)

// Seeder generates per-tenant-per-app credentials and persists them
// write-once to OpenBao. Reconcilers hold a *Seeder, call the
// category-specific method before creating their provisioning Job, and pass
// the returned credential struct to the Job via an env var.
//
// Generation strategy, in ModeDerived: HKDF-SHA256(master, salt=KV path,
// info=field) — fully deterministic, so an app uninstalled and reinstalled
// gets identical credentials back without external state. Tenant-app salts
// include the tenant component (CategoryPath/InternalPath); kernel-shared
// salts do not (KernelPath). When the master password is unavailable the
// seeder falls back to crypto/rand.
//
// In ModeRandom every value is drawn from crypto/rand and the master password
// is not used, whether or not the Seeder holds it.
//
// In both modes the first value stored at a path stays the path's value: a
// later pass reads it back and never replaces it. That is what keeps a random
// credential the same over reconciles and restarts, and what makes a change
// of mode leave every credential that exists as it is — only a credential
// made afterwards follows the new mode.
type Seeder struct {
	w    Writer
	d    *Deriver
	mode ModeFunc
}

// NewSeeder constructs a Seeder backed by w (typically a *KVClient) and
// derivation backed by d. d may be nil, in which case credentials are
// generated with crypto/rand instead of derived. The Seeder is in
// ModeDerived until WithMode says where the mode is read from.
func NewSeeder(w Writer, d *Deriver) *Seeder {
	return &Seeder{w: w, d: d}
}

// WithMode makes the Seeder ask mode before it makes a value, and returns it.
func (s *Seeder) WithMode(mode ModeFunc) *Seeder {
	s.mode = mode
	return s
}

// KV returns the KV client backing this Seeder, or nil when the Writer is
// something else (a fake, in tests). Per-tenant auth mounts reuse this
// session rather than opening a second one and holding a second identity.
func (s *Seeder) KV() *KVClient {
	kv, _ := s.w.(*KVClient)
	return kv
}

// derives reports whether a value made now is computed from the master
// password, and so comes out the same whenever it is made again.
func (s *Seeder) derives(ctx context.Context) (bool, error) {
	mode := ModeDerived
	if s.mode != nil {
		m, err := s.mode(ctx)
		if err != nil {
			return false, fmt.Errorf("secret mode: %w", err)
		}
		mode = m
	}
	switch mode {
	case ModeDerived:
		return s.d != nil && s.d.HasMaster(), nil
	case ModeRandom:
		return false, nil
	default:
		return false, fmt.Errorf("secret mode: %q is neither %q nor %q", mode, ModeDerived, ModeRandom)
	}
}

// generated is a value the Seeder made, and whether making it again would
// give the same one.
type generated struct {
	value        string
	reproducible bool
}

// gen returns n hex characters: HKDF-derived from (salt, info) in ModeDerived
// when a master is configured, otherwise crypto/rand.
func (s *Seeder) gen(ctx context.Context, salt, info string, n int) (generated, error) {
	if n <= 0 {
		n = 40
	}
	derives, err := s.derives(ctx)
	if err != nil {
		return generated{}, err
	}
	if derives {
		return generated{value: s.d.Derive(salt, info, n), reproducible: true}, nil
	}
	return generated{value: randomHex(n)}, nil
}

// randomHex returns n hex characters from crypto/rand.
func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		panic("secrets.Seeder: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(buf)[:n]
}

// seedAndRead writes data with PutOnce, then re-reads to honour any value
// already present (manual overrides or values from a prior reconcile).
//
// reproducible says the values in data would come out the same if made again;
// only then may they stand in for a read that failed. A random value that was
// not read back may not be the stored one — PutOnce leaves an existing path
// alone — and handing it on would set a database role or a bucket user to a
// password nothing holds.
func (s *Seeder) seedAndRead(ctx context.Context, path string, data map[string]string, reproducible bool) (map[string]string, error) {
	if err := s.w.PutOnce(ctx, path, data); err != nil {
		return nil, err
	}
	out, err := s.w.Get(ctx, path)
	if err != nil {
		if !reproducible {
			return nil, fmt.Errorf("read back %s: %w", path, err)
		}
		return data, nil //nolint:nilerr // value was written; tolerate read failure
	}
	return out, nil
}

// --- OIDC --------------------------------------------------------------------

// OIDCCreds is the set of values written to …/oidc.
type OIDCCreds struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

// SeedOIDC derives the OIDC client secret for (tenant, app) from the master,
// persists the full record, and returns the effective credentials. Issuer and
// client-id are refreshed on reconcile; client-secret is write-once.
func (s *Seeder) SeedOIDC(ctx context.Context, tenant, app, issuer, clientID string) (OIDCCreds, error) {
	salt := CategoryPath(tenant, app, "oidc")
	existing, _ := s.w.Get(ctx, salt)
	// The stored secret stands; one is made only where none is stored.
	secret, certain := "", true
	if existing != nil {
		secret = existing["client-secret"]
	}
	if secret == "" {
		g, err := s.gen(ctx, salt, "client-secret", 40)
		if err != nil {
			return OIDCCreds{}, fmt.Errorf("seed oidc(%s/%s): %w", tenant, app, err)
		}
		secret, certain = g.value, g.reproducible || existing != nil
	}
	want := map[string]string{
		"issuer":        issuer,
		"client-id":     clientID,
		"client-secret": secret,
	}
	var err error
	if existing == nil {
		err = s.w.PutOnce(ctx, salt, want)
	} else {
		err = s.w.Put(ctx, salt, want)
	}
	if err != nil {
		return OIDCCreds{}, fmt.Errorf("seed oidc(%s/%s): %w", tenant, app, err)
	}
	got, err := s.w.Get(ctx, salt)
	if err != nil {
		// A random secret offered to a path that may already have held one
		// is not known to be the stored one until it has been read back.
		if !certain {
			return OIDCCreds{}, fmt.Errorf("seed oidc(%s/%s): read back: %w", tenant, app, err)
		}
		got = want
	}
	return OIDCCreds{
		Issuer:       got["issuer"],
		ClientID:     got["client-id"],
		ClientSecret: got["client-secret"],
	}, nil
}

// --- Database (PostgreSQL flavour) -------------------------------------------

// DatabaseCreds is the set of values written to …/database.
type DatabaseCreds struct {
	Host     string
	Port     string
	Name     string
	User     string
	Password string
}

// SeedKernelDatabase derives a kernel-scoped database password and writes the
// connection record under gentian-os/kernel/database/{category}.
func (s *Seeder) SeedKernelDatabase(ctx context.Context, category string, conn DatabaseCreds) (DatabaseCreds, error) {
	salt := KernelPath("database", category)
	reproducible := true
	if conn.Password == "" {
		g, err := s.gen(ctx, salt, "password", 40)
		if err != nil {
			return DatabaseCreds{}, fmt.Errorf("seed kernel database(%s): %w", category, err)
		}
		conn.Password, reproducible = g.value, g.reproducible
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"host":     conn.Host,
		"port":     conn.Port,
		"name":     conn.Name,
		"user":     conn.User,
		"password": conn.Password,
	}, reproducible)
	if err != nil {
		return DatabaseCreds{}, fmt.Errorf("seed kernel database(%s): %w", category, err)
	}
	return DatabaseCreds{
		Host: got["host"], Port: got["port"], Name: got["name"],
		User: got["user"], Password: got["password"],
	}, nil
}

// SeedDatabase derives the role password and writes the connection record.
// host/port/name/user are supplied by the caller.
func (s *Seeder) SeedDatabase(ctx context.Context, tenant, app string, conn DatabaseCreds) (DatabaseCreds, error) {
	salt := CategoryPath(tenant, app, "database")
	reproducible := true
	if conn.Password == "" {
		g, err := s.gen(ctx, salt, "password", 40)
		if err != nil {
			return DatabaseCreds{}, fmt.Errorf("seed database(%s/%s): %w", tenant, app, err)
		}
		conn.Password, reproducible = g.value, g.reproducible
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"host":     conn.Host,
		"port":     conn.Port,
		"name":     conn.Name,
		"user":     conn.User,
		"password": conn.Password,
	}, reproducible)
	if err != nil {
		return DatabaseCreds{}, fmt.Errorf("seed database(%s/%s): %w", tenant, app, err)
	}
	return DatabaseCreds{
		Host: got["host"], Port: got["port"], Name: got["name"],
		User: got["user"], Password: got["password"],
	}, nil
}

// SeedMariaDB writes the MariaDB connection record under the same "database"
// category — apps with kernelRequirements.database pick the engine and the
// reconciler reads by category name.
func (s *Seeder) SeedMariaDB(ctx context.Context, tenant, app string, conn DatabaseCreds) (DatabaseCreds, error) {
	return s.SeedDatabase(ctx, tenant, app, conn)
}

// --- S3 / MinIO --------------------------------------------------------------

// S3Creds is the set of values written to …/s3.
type S3Creds struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

// SeedS3 derives the access/secret key pair from the master.
func (s *Seeder) SeedS3(ctx context.Context, tenant, app string, base S3Creds) (S3Creds, error) {
	salt := CategoryPath(tenant, app, "s3")
	reproducible := true
	if base.AccessKey == "" {
		g, err := s.gen(ctx, salt, "access-key", 20)
		if err != nil {
			return S3Creds{}, fmt.Errorf("seed s3(%s/%s): %w", tenant, app, err)
		}
		base.AccessKey, reproducible = g.value, g.reproducible
	}
	if base.SecretKey == "" {
		g, err := s.gen(ctx, salt, "secret-key", 40)
		if err != nil {
			return S3Creds{}, fmt.Errorf("seed s3(%s/%s): %w", tenant, app, err)
		}
		base.SecretKey, reproducible = g.value, reproducible && g.reproducible
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"endpoint":   base.Endpoint,
		"bucket":     base.Bucket,
		"region":     base.Region,
		"access-key": base.AccessKey,
		"secret-key": base.SecretKey,
	}, reproducible)
	if err != nil {
		return S3Creds{}, fmt.Errorf("seed s3(%s/%s): %w", tenant, app, err)
	}
	return S3Creds{
		Endpoint: got["endpoint"], Bucket: got["bucket"], Region: got["region"],
		AccessKey: got["access-key"], SecretKey: got["secret-key"],
	}, nil
}

// --- Cache (Redis) -----------------------------------------------------------

// CacheCreds is the set of values written to …/cache.
type CacheCreds struct {
	Host     string
	Port     string
	User     string
	Password string
}

// SeedCache derives the cache password from the master. Host and port are refreshed
// on reconcile so infrastructure moves (e.g. shared Redis in gentian-infra-dev) propagate.
//
// User is the per-app ACL user the cache reconciler provisions. It is derived from
// the tenant and app names rather than generated, but it is recorded here so apps can
// consume it through valueMapping.cache.userKey instead of reconstructing the naming
// rule in every profile. Engines without per-app users (memcached) leave it empty.
func (s *Seeder) SeedCache(ctx context.Context, tenant, app string, base CacheCreds) (CacheCreds, error) {
	salt := CategoryPath(tenant, app, "cache")
	existing, _ := s.w.Get(ctx, salt)
	password, certain := base.Password, true
	if password == "" {
		if existing != nil && existing["password"] != "" {
			password = existing["password"]
		} else {
			g, err := s.gen(ctx, salt, "password", 40)
			if err != nil {
				return CacheCreds{}, fmt.Errorf("seed cache(%s/%s): %w", tenant, app, err)
			}
			password, certain = g.value, g.reproducible || existing != nil
		}
	}
	want := map[string]string{
		"host":     base.Host,
		"port":     base.Port,
		"password": password,
	}
	// Only record a user for engines that have one; never clobber a stored value
	// with an empty string when the caller does not supply it.
	if base.User != "" {
		want["user"] = base.User
	} else if existing != nil && existing["user"] != "" {
		want["user"] = existing["user"]
	}
	var err error
	if existing == nil {
		err = s.w.PutOnce(ctx, salt, want)
	} else {
		if existing["password"] != "" {
			want["password"] = existing["password"]
		}
		err = s.w.Put(ctx, salt, want)
	}
	if err != nil {
		return CacheCreds{}, fmt.Errorf("seed cache(%s/%s): %w", tenant, app, err)
	}
	got, err := s.w.Get(ctx, salt)
	if err != nil {
		// As for the OIDC secret: a random password is only the app's once it
		// has been read back from the path.
		if !certain {
			return CacheCreds{}, fmt.Errorf("seed cache(%s/%s): read back: %w", tenant, app, err)
		}
		return CacheCreds{
			Host: want["host"], Port: want["port"], User: want["user"], Password: want["password"],
		}, nil //nolint:nilerr
	}
	return CacheCreds{
		Host: got["host"], Port: got["port"], User: got["user"], Password: got["password"],
	}, nil
}

// --- Model gateway -----------------------------------------------------------

// ModelAccessCategory is the category an app's model gateway record is kept
// under, beside its database and its cache.
const ModelAccessCategory = "llm"

// ModelAccessCreds is the set of values written to …/llm.
type ModelAccessCreds struct {
	// BaseURL is the gateway's OpenAI-compatible address.
	BaseURL string
	// APIKey is the key the app presents.
	APIKey string
}

// SeedModelAccess makes the key an app presents to the model gateway and
// records it with the gateway's address. The key is generated as a database
// password is: derived from the master for this tenant and app, or random
// where there is no master, and write-once either way -- so an app
// uninstalled and installed again holds the key it held, and nothing about
// the key follows from the tenant's and the app's names. prefix is what the
// gateway requires a key to begin with. The address is refreshed on every
// pass, so a gateway that moves is followed.
func (s *Seeder) SeedModelAccess(ctx context.Context, tenant, app, prefix, baseURL string) (ModelAccessCreds, error) {
	salt := CategoryPath(tenant, app, ModelAccessCategory)
	existing, _ := s.w.Get(ctx, salt)
	key := ""
	if existing != nil {
		key = existing["api-key"]
	}
	if key == "" {
		g, err := s.gen(ctx, salt, "api-key", 48)
		if err != nil {
			return ModelAccessCreds{}, fmt.Errorf("seed model access(%s/%s): %w", tenant, app, err)
		}
		key = prefix + g.value
	}
	want := map[string]string{"base-url": baseURL, "api-key": key}
	var err error
	if existing == nil {
		err = s.w.PutOnce(ctx, salt, want)
	} else if existing["base-url"] != baseURL || existing["api-key"] != key {
		err = s.w.Put(ctx, salt, want)
	}
	if err != nil {
		return ModelAccessCreds{}, fmt.Errorf("seed model access(%s/%s): %w", tenant, app, err)
	}
	// Read back: what the vault holds is what the app's ExternalSecret
	// delivers, and so what has to be registered at the gateway. A key seeded
	// by another pass in the meantime wins over the one made here.
	got, err := s.w.Get(ctx, salt)
	if err != nil || got["api-key"] == "" {
		return ModelAccessCreds{}, fmt.Errorf("seed model access(%s/%s): the vault does not give the record back: %v", tenant, app, err)
	}
	return ModelAccessCreds{BaseURL: got["base-url"], APIKey: got["api-key"]}, nil
}

// --- SMTP --------------------------------------------------------------------

// SMTPCreds is the set of values written to …/smtp.
type SMTPCreds struct {
	Host     string
	Port     string
	User     string
	Password string
}

// SeedSMTP writes the SMTP record. The password is *copied* from a
// kernel-level relay secret rather than derived (the relay is shared
// across tenants); callers pass the already-retrieved value in.
func (s *Seeder) SeedSMTP(ctx context.Context, tenant, app string, base SMTPCreds) (SMTPCreds, error) {
	got, err := s.seedAndRead(ctx, CategoryPath(tenant, app, "smtp"), map[string]string{
		"host":     base.Host,
		"port":     base.Port,
		"user":     base.User,
		"password": base.Password,
	}, true)
	if err != nil {
		return SMTPCreds{}, fmt.Errorf("seed smtp(%s/%s): %w", tenant, app, err)
	}
	return SMTPCreds{Host: got["host"], Port: got["port"], User: got["user"], Password: got["password"]}, nil
}

// --- IMAP --------------------------------------------------------------------

// IMAPCreds is the set of values written to …/imap.
type IMAPCreds struct {
	Host string
	Port string
}

// SeedIMAP writes host/port only; per-user IMAP credentials come from Keycloak at runtime.
func (s *Seeder) SeedIMAP(ctx context.Context, tenant, app string, base IMAPCreds) error {
	return s.w.PutOnce(ctx, CategoryPath(tenant, app, "imap"), map[string]string{
		"host": base.Host,
		"port": base.Port,
	})
}

// --- IMAP --------------------------------------------------------------------

// --- Per-app internal secrets ------------------------------------------------

// SeedAppSecret makes a single AppSecret by name and writes it as
// {"value": "<value>"} under …/internal/{name}: derived or random as the
// mode says, and the first value stored stays the path's.
//
// An app encrypts and signs its own data with these, so the data is readable
// only with the value it was written with. A bundle carries the values with
// the data (bundle.ArtefactSecrets) and a restore sets them
// (ReplaceAppSecret): that, and not making the value again, is what brings
// an app's data back readable after a purge, in another tenant or on another
// cluster, in both modes.
func (s *Seeder) SeedAppSecret(ctx context.Context, tenant, app, name string) (string, error) {
	salt := InternalPath(tenant, app, name)
	g, err := s.gen(ctx, salt, "value", 40)
	if err != nil {
		return "", fmt.Errorf("seed app-secret(%s/%s/%s): %w", tenant, app, name, err)
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"value": g.value,
	}, g.reproducible)
	if err != nil {
		return "", fmt.Errorf("seed app-secret(%s/%s/%s): %w", tenant, app, name, err)
	}
	return got["value"], nil
}

// ReadAppSecret reads the stored value of one of an app's own secrets. found
// is false where the path holds nothing; an error is a path that could not
// be read, which is not the same.
func (s *Seeder) ReadAppSecret(ctx context.Context, tenant, app, name string) (value string, found bool, err error) {
	got, err := s.w.Get(ctx, InternalPath(tenant, app, name))
	if errors.Is(err, ErrNotFound) || (err == nil && got == nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read app-secret(%s/%s/%s): %w", tenant, app, name, err)
	}
	return got["value"], got["value"] != "", nil
}

// ReplaceAppSecret sets one of an app's own secrets to value, whatever the
// path held, and reports whether that changed it. It reads the path back
// and fails unless it then holds value.
//
// This is the one place a stored value the platform generated is replaced,
// and it is a restore's: the data a bundle brings back was written with the
// bundle's value, and the value the path holds -- made new when the app was
// installed again, or for another tenant -- cannot read it. The path is
// built here from the tenant, the app and the name; a caller hands over no
// path.
func (s *Seeder) ReplaceAppSecret(ctx context.Context, tenant, app, name, value string) (changed bool, err error) {
	if value == "" {
		return false, fmt.Errorf("replace app-secret(%s/%s/%s): no value", tenant, app, name)
	}
	have, found, err := s.ReadAppSecret(ctx, tenant, app, name)
	if err != nil {
		return false, err
	}
	if found && have == value {
		return false, nil
	}
	path := InternalPath(tenant, app, name)
	if err := s.w.Put(ctx, path, map[string]string{"value": value}); err != nil {
		return false, fmt.Errorf("replace app-secret(%s/%s/%s): %w", tenant, app, name, err)
	}
	got, err := s.w.Get(ctx, path)
	if err != nil || got["value"] != value {
		return false, fmt.Errorf("replace app-secret(%s/%s/%s): the vault does not give the value back: %v", tenant, app, name, err)
	}
	return true, nil
}

// --- Contracts ---------------------------------------------------------------

// ContractCreds represents the credentials shared between an integration provider and consumer.
type ContractCreds struct {
	Password string
}

// SeedContract writes a unique password into OpenBao for an integration contract.
func (s *Seeder) SeedContract(ctx context.Context, tenant, contract string) (ContractCreds, error) {
	salt := ContractPath(tenant, contract)
	g, err := s.gen(ctx, salt, "password", 40)
	if err != nil {
		return ContractCreds{}, fmt.Errorf("seed contract(%s/%s): %w", tenant, contract, err)
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"password": g.value,
	}, g.reproducible)
	if err != nil {
		return ContractCreds{}, fmt.Errorf("seed contract(%s/%s): %w", tenant, contract, err)
	}
	return ContractCreds{Password: got["password"]}, nil
}

func (s *Seeder) Read(ctx context.Context, logicalPath string) (map[string]string, error) {
	return s.w.Get(ctx, logicalPath)
}

func (s *Seeder) Write(ctx context.Context, logicalPath string, data map[string]string) error {
	return s.w.Put(ctx, logicalPath, data)
}
