/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/


package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Writer is the narrow slice of KVClient that the Seeder needs. It exists so
// reconciler tests can substitute an in-memory fake.
type Writer interface {
	PutOnce(ctx context.Context, logicalPath string, data map[string]string) error
	Put(ctx context.Context, logicalPath string, data map[string]string) error
	Get(ctx context.Context, logicalPath string) (map[string]string, error)
}

// Seeder generates per-tenant-per-app credentials and persists them
// write-once to OpenBao. Reconcilers hold a *Seeder, call the
// category-specific method before creating their provisioning Job, and pass
// the returned credential struct to the Job via an env var.
//
// Generation strategy: HKDF-SHA256(master, salt=KV path, info=field) — fully
// deterministic, so an app uninstalled and reinstalled gets identical
// credentials back without external state. Tenant-app salts include the
// tenant component (CategoryPath/InternalPath); kernel-shared salts do not
// (KernelPath). When the master password is unavailable the seeder falls
// back to crypto/rand and PutOnce semantics keep the first random value
// authoritative.
type Seeder struct {
	w Writer
	d *Deriver
}

// NewSeeder constructs a Seeder backed by w (typically a *KVClient) and
// derivation backed by d. d may be nil, in which case credentials are
// generated with crypto/rand instead of derived.
func NewSeeder(w Writer, d *Deriver) *Seeder {
	return &Seeder{w: w, d: d}
}

// gen returns n hex characters: HKDF-derived from (salt, info) when a master
// is configured, otherwise crypto/rand.
func (s *Seeder) gen(salt, info string, n int) string {
	if n <= 0 {
		n = 40
	}
	if s.d != nil && s.d.HasMaster() {
		return s.d.Derive(salt, info, n)
	}
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		panic("secrets.Seeder.gen: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(buf)[:n]
}

// genTenantAdminPassword returns a password with mixed character classes.
// Pure-hex derived passwords are rejected on PATCH even when CREATE succeeded on an earlier deploy.
func (s *Seeder) genTenantAdminPassword(salt string) string {
	return "Gt!" + s.gen(salt, "password", 24)
}

// seedAndRead writes data with PutOnce, then re-reads to honour any value
// already present (manual overrides or values from a prior reconcile).
func (s *Seeder) seedAndRead(ctx context.Context, path string, data map[string]string) (map[string]string, error) {
	if err := s.w.PutOnce(ctx, path, data); err != nil {
		return nil, err
	}
	out, err := s.w.Get(ctx, path)
	if err != nil {
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
	secret := s.gen(salt, "client-secret", 40)
	if existing != nil {
		if v := existing["client-secret"]; v != "" {
			secret = v
		}
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
	if conn.Password == "" {
		conn.Password = s.gen(salt, "password", 40)
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"host":     conn.Host,
		"port":     conn.Port,
		"name":     conn.Name,
		"user":     conn.User,
		"password": conn.Password,
	})
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
	if conn.Password == "" {
		conn.Password = s.gen(salt, "password", 40)
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"host":     conn.Host,
		"port":     conn.Port,
		"name":     conn.Name,
		"user":     conn.User,
		"password": conn.Password,
	})
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
	if base.AccessKey == "" {
		base.AccessKey = s.gen(salt, "access-key", 20)
	}
	if base.SecretKey == "" {
		base.SecretKey = s.gen(salt, "secret-key", 40)
	}
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"endpoint":   base.Endpoint,
		"bucket":     base.Bucket,
		"region":     base.Region,
		"access-key": base.AccessKey,
		"secret-key": base.SecretKey,
	})
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
	password := base.Password
	if password == "" {
		if existing != nil && existing["password"] != "" {
			password = existing["password"]
		} else {
			password = s.gen(salt, "password", 40)
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
		return CacheCreds{
			Host: want["host"], Port: want["port"], User: want["user"], Password: want["password"],
		}, nil //nolint:nilerr
	}
	return CacheCreds{
		Host: got["host"], Port: got["port"], User: got["user"], Password: got["password"],
	}, nil
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
	})
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

// TenantAdminCreds is the set of values written to …/admin.
type TenantAdminCreds struct {
	Username string
	Password string
}

// SeedTenantAdmin derives the tenant admin password from the master and
// persists it write-once under gentian-os/tenants/<tenant>/admin.
// Username defaults to "admin-<tenant>". Operators can override it by writing
// a different value to OpenBao before first reconcile.
func (s *Seeder) SeedTenantAdmin(ctx context.Context, tenant string) (TenantAdminCreds, error) {
	salt := TenantAdminPath(tenant)
	want := map[string]string{
		"username": "admin-" + tenant,
		"password": s.genTenantAdminPassword(salt),
	}
	got, err := s.seedAndRead(ctx, salt, want)
	if err != nil {
		return TenantAdminCreds{}, fmt.Errorf("seed tenant-admin(%s): %w", tenant, err)
	}
	return TenantAdminCreds{
		Username: got["username"],
		Password: got["password"],
	}, nil
}

// --- Per-app internal secrets ------------------------------------------------

// SeedAppSecret derives a single AppSecret by name and writes it as
// {"value": "<derived>"} under …/internal/{name}.
func (s *Seeder) SeedAppSecret(ctx context.Context, tenant, app, name string) (string, error) {
	salt := InternalPath(tenant, app, name)
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"value": s.gen(salt, "value", 40),
	})
	if err != nil {
		return "", fmt.Errorf("seed app-secret(%s/%s/%s): %w", tenant, app, name, err)
	}
	return got["value"], nil
}

// --- Contracts ---------------------------------------------------------------

// ContractCreds represents the credentials shared between an integration provider and consumer.
type ContractCreds struct {
	Password string
}

// SeedContract writes a unique password into OpenBao for an integration contract.
func (s *Seeder) SeedContract(ctx context.Context, tenant, contract string) (ContractCreds, error) {
	salt := ContractPath(tenant, contract)
	got, err := s.seedAndRead(ctx, salt, map[string]string{
		"password": s.gen(salt, "password", 40),
	})
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
