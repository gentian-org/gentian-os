/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package vaultmigrate is what a release changes in the vault.
//
// No root token survives handover, so nothing the installer writes can change
// on a running cluster. This runs in the kernel chart's vault-migrator Job as
// its own identity, on every release, and each step leaves alone what is
// already right.
package vaultmigrate

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

//go:embed crossplane-write.hcl
var crossplaneWrite string

const (
	policyPath = "sys/policies/acl/crossplane-write"
	rolePath   = "pki-internal/roles/gentian-ca"
	// policyMark is the line every crossplane-write has held. A policy without
	// it is not one this package wrote, and is not replaced.
	policyMark = `path "sys/policies/acl/*"`
)

// Vault is the vault as this package uses it.
type Vault interface {
	Request(ctx context.Context, method, path string, body any) (int, []byte, error)
	Get(ctx context.Context, logicalPath string) (map[string]string, error)
}

// Seed is one key a release adds to a kernel path that exists. Context and
// Purpose are what the installer derives the key from (scripts/lib/bootstrap.sh,
// _derived), so a cluster that gets it here holds what a new one would.
type Seed struct {
	Path, Key, Context, Purpose string
}

// Seeds are the keys added since the first release. A key already there stays
// as it is, however it got there.
var Seeds = []Seed{
	{"gentian-os/kernel/authz/openfga", "director_key", "openfga", "director_key"},
	{"gentian-os/kernel/authz/openfga", "bouncer_key", "openfga", "bouncer_key"},
	{"gentian-os/kernel/authz/openfga", "usher_key", "openfga", "usher_key"},
	{"gentian-os/kernel/authz/openfga", "registrar_key", "openfga", "registrar_key"},
	{"gentian-os/kernel/authz/openfga", "custodian_key", "openfga", "custodian_key"},
	{"gentian-os/kernel/identity/keycloak-bootstrap", "operator_client_secret", "keycloak", "operatorClientSecret"},
}

// CrossplaneWrite is the policy for a KV mount.
func CrossplaneWrite(mount string) string {
	return strings.ReplaceAll(crossplaneWrite, "__KV_MOUNT__", mount)
}

// Run makes the vault what this release needs. The first step that fails ends
// it, and the next run starts over; every step is safe to repeat.
func Run(ctx context.Context, v Vault, mount string, names []string, seeds []Seed, log func(string, ...any)) error {
	if err := policy(ctx, v, mount, log); err != nil {
		return err
	}
	if err := caNames(ctx, v, names, log); err != nil {
		return err
	}
	return addKeys(ctx, v, mount, seeds, log)
}

func policy(ctx context.Context, v Vault, mount string, log func(string, ...any)) error {
	status, raw, err := v.Request(ctx, http.MethodGet, policyPath, nil)
	if err != nil {
		return fmt.Errorf("read crossplane-write: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("read crossplane-write: HTTP %d: %s", status, raw)
	}
	var out struct {
		Data struct {
			Policy string `json:"policy"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("read crossplane-write: %w", err)
	}
	want := CrossplaneWrite(mount)
	if out.Data.Policy == want {
		log("crossplane-write is current")
		return nil
	}
	if !strings.Contains(out.Data.Policy, policyMark) {
		return errors.New("crossplane-write is not a policy this release knows, and is left as it is: " +
			"compare it with internal/vaultmigrate/crossplane-write.hcl")
	}
	status, raw, err = v.Request(ctx, http.MethodPut, policyPath, map[string]string{"policy": want})
	if err != nil {
		return fmt.Errorf("write crossplane-write: %w", err)
	}
	if status/100 != 2 {
		return fmt.Errorf("write crossplane-write: HTTP %d: %s", status, raw)
	}
	log("crossplane-write written")
	return nil
}

// caNames adds names to what gentian-ca signs. Only adds: the installer sets
// the role by the issuer mode, and a name it put there stays. A cluster whose
// CA is the person's own has no role, and nothing to do.
func caNames(ctx context.Context, v Vault, names []string, log func(string, ...any)) error {
	status, raw, err := v.Request(ctx, http.MethodGet, rolePath, nil)
	if err != nil {
		return fmt.Errorf("read gentian-ca: %w", err)
	}
	if status == http.StatusNotFound {
		log("gentian-ca has no role here")
		return nil
	}
	if status != http.StatusOK {
		return fmt.Errorf("read gentian-ca: HTTP %d: %s", status, raw)
	}
	var out struct {
		Data struct {
			AllowedDomains []string `json:"allowed_domains"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("read gentian-ca: %w", err)
	}
	domains := out.Data.AllowedDomains
	added := false
	for _, name := range names {
		if !slices.Contains(domains, name) {
			domains = append(domains, name)
			added = true
		}
	}
	if !added {
		log("gentian-ca names are current")
		return nil
	}
	status, raw, err = v.Request(ctx, http.MethodPatch, rolePath, map[string]any{"allowed_domains": domains})
	if err != nil {
		return fmt.Errorf("write gentian-ca: %w", err)
	}
	if status/100 != 2 {
		return fmt.Errorf("write gentian-ca: HTTP %d: %s", status, raw)
	}
	log("gentian-ca names written")
	return nil
}

func addKeys(ctx context.Context, v Vault, mount string, seeds []Seed, log func(string, ...any)) error {
	master, salt := "", ""
	switch stored, err := v.Get(ctx, secrets.MasterPasswordPath); {
	case err == nil:
		master, salt = stored["value"], stored["salt"]
	case !errors.Is(err, secrets.ErrNotFound):
		return err
	}
	for _, seed := range seeds {
		have, err := v.Get(ctx, seed.Path)
		if err != nil {
			return err
		}
		if have[seed.Key] != "" {
			continue
		}
		value, err := derive(master, salt, seed)
		if err != nil {
			return err
		}
		path := fmt.Sprintf("%s/data/%s", mount, seed.Path)
		status, raw, err := v.Request(ctx, http.MethodPatch, path, map[string]any{
			"options": map[string]any{},
			"data":    map[string]string{seed.Key: value},
		})
		if err != nil {
			return fmt.Errorf("add %s to %s: %w", seed.Key, seed.Path, err)
		}
		if status/100 != 2 {
			return fmt.Errorf("add %s to %s: HTTP %d: %s", seed.Key, seed.Path, status, raw)
		}
		log("key added", "path", seed.Path, "key", seed.Key)
	}
	return nil
}

// derive is the installer's _derived. A cluster with no master password keeps
// no way to derive one, and gets a random key, as secretMode random does.
func derive(master, salt string, seed Seed) (string, error) {
	if master == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		return hex.EncodeToString(raw), nil
	}
	mac := hmac.New(sha256.New, []byte(master+salt))
	mac.Write([]byte(seed.Context + ":" + seed.Purpose))
	return hex.EncodeToString(mac.Sum(nil)), nil
}
