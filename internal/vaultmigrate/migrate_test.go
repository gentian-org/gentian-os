/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package vaultmigrate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

type fakeVault struct {
	policy  string
	domains []string
	hasRole bool
	kv      map[string]map[string]string
	writes  []string
}

func (f *fakeVault) Get(_ context.Context, path string) (map[string]string, error) {
	data, ok := f.kv[path]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return data, nil
}

func (f *fakeVault) Request(_ context.Context, method, path string, body any) (int, []byte, error) {
	switch {
	case path == policyPath && method == http.MethodGet:
		raw, _ := json.Marshal(map[string]any{"data": map[string]string{"policy": f.policy}})
		return http.StatusOK, raw, nil
	case path == policyPath:
		f.policy = body.(map[string]string)["policy"]
	case path == rolePath && !f.hasRole:
		return http.StatusNotFound, nil, nil
	case path == rolePath && method == http.MethodGet:
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"allowed_domains": f.domains}})
		return http.StatusOK, raw, nil
	case path == rolePath:
		f.domains = body.(map[string]any)["allowed_domains"].([]string)
	default:
		logical := strings.TrimPrefix(path, "secret/data/")
		for k, v := range body.(map[string]any)["data"].(map[string]string) {
			f.kv[logical][k] = v
		}
	}
	f.writes = append(f.writes, path)
	return http.StatusOK, nil, nil
}

func quiet(string, ...any) {}

func cluster() *fakeVault {
	return &fakeVault{
		policy:  `path "sys/policies/acl/*" { capabilities = ["update"] }`,
		domains: []string{"seaweedfs-server"},
		hasRole: true,
		kv: map[string]map[string]string{
			secrets.MasterPasswordPath: {"value": "master", "salt": "salt"},
			"p":                        {"have": "rotated"},
		},
	}
}

func TestAReleaseIsBroughtIn(t *testing.T) {
	v := cluster()
	seeds := []Seed{{"p", "have", "c", "x"}, {"p", "new", "openfga", "director_key"}}
	if err := Run(context.Background(), v, "secret", []string{"mariadb.x.svc"}, seeds, quiet); err != nil {
		t.Fatal(err)
	}
	if v.policy != CrossplaneWrite("secret") {
		t.Errorf("policy not written: %s", v.policy)
	}
	if strings.Join(v.domains, ",") != "seaweedfs-server,mariadb.x.svc" {
		t.Errorf("domains: %v", v.domains)
	}
	if v.kv["p"]["have"] != "rotated" {
		t.Error("a key that was there was replaced")
	}
	// The installer's _derived, as openssl computes it.
	out, err := exec.Command("sh", "-c",
		"printf 'openfga:director_key' | openssl dgst -sha256 -hmac mastersalt | awk '{print $NF}'").Output()
	if err != nil {
		t.Skip("no openssl here")
	}
	if want := strings.TrimSpace(string(out)); v.kv["p"]["new"] != want {
		t.Errorf("derived %s, the installer derives %s", v.kv["p"]["new"], want)
	}
}

func TestASecondRunWritesNothing(t *testing.T) {
	v := cluster()
	seeds := []Seed{{"p", "new", "openfga", "director_key"}}
	names := []string{"mariadb.x.svc"}
	for i := range 2 {
		if i == 1 {
			v.writes = nil
		}
		if err := Run(context.Background(), v, "secret", names, seeds, quiet); err != nil {
			t.Fatal(err)
		}
	}
	if len(v.writes) != 0 {
		t.Errorf("wrote %v", v.writes)
	}
}

func TestAPolicyThisReleaseDoesNotKnowIsLeftAlone(t *testing.T) {
	v := cluster()
	v.policy = `path "secret/*" { capabilities = ["read"] }`
	if err := Run(context.Background(), v, "secret", nil, nil, quiet); err == nil {
		t.Fatal("replaced a policy it does not know")
	}
	if len(v.writes) != 0 {
		t.Errorf("wrote %v", v.writes)
	}
}

func TestACAThatIsThePersonsOwnIsLeftAlone(t *testing.T) {
	v := cluster()
	v.hasRole = false
	if err := Run(context.Background(), v, "secret", []string{"a"}, nil, quiet); err != nil {
		t.Fatal(err)
	}
}

func TestCrossplaneMayNotChangeItsOwnRights(t *testing.T) {
	text := CrossplaneWrite("kv")
	for _, want := range []string{
		fmt.Sprintf("path %q", "kv/data/gentian-os/*"),
		`path "sys/policies/acl/crossplane-write"  { capabilities = ["read"] }`,
		`path "pki-internal/roles/gentian-ca"      { capabilities = ["deny"] }`,
		policyMark,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("crossplane-write lacks %s", want)
		}
	}
	if strings.Contains(text, "__KV_MOUNT__") {
		t.Error("the mount was not filled in")
	}
}
