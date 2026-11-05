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
	"strings"
	"testing"
)

type fakeVault struct {
	policy  string
	domains []string
	hasRole bool
	writes  []string
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
		return http.StatusForbidden, nil, nil
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
	}
}

func TestAReleaseIsBroughtIn(t *testing.T) {
	v := cluster()
	if err := Run(context.Background(), v, "secret", []string{"mariadb.x.svc"}, quiet); err != nil {
		t.Fatal(err)
	}
	if v.policy != CrossplaneWrite("secret") {
		t.Errorf("policy not written: %s", v.policy)
	}
	if strings.Join(v.domains, ",") != "seaweedfs-server,mariadb.x.svc" {
		t.Errorf("domains: %v", v.domains)
	}
}

func TestASecondRunWritesNothing(t *testing.T) {
	v := cluster()
	names := []string{"mariadb.x.svc"}
	for i := range 2 {
		if i == 1 {
			v.writes = nil
		}
		if err := Run(context.Background(), v, "secret", names, quiet); err != nil {
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
	if err := Run(context.Background(), v, "secret", nil, quiet); err == nil {
		t.Fatal("replaced a policy it does not know")
	}
	if len(v.writes) != 0 {
		t.Errorf("wrote %v", v.writes)
	}
}

func TestACAThatIsThePersonsOwnIsLeftAlone(t *testing.T) {
	v := cluster()
	v.hasRole = false
	if err := Run(context.Background(), v, "secret", []string{"a"}, quiet); err != nil {
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
