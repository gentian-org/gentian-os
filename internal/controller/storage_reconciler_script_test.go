/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"os/exec"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The backup bucket is deleted as its own unit, and the pseudo-app name the
// Job carries resolves to exactly the bucket exports write to.
func TestBackupBucketUnitNamesTheBackupBucket(t *testing.T) {
	for _, tenant := range []*gentianov1alpha1.Tenant{
		{ObjectMeta: metav1.ObjectMeta{Name: "acme"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: gentianov1alpha1.TenantSpec{
			Isolation: &gentianov1alpha1.TenantIsolation{S3Prefix: "Corp_"}}},
	} {
		if got, want := s3BucketName(tenant, backupBucketUnit), backup.BackupBucket(tenant); got != want {
			t.Fatalf("backup bucket unit = %q, exports write to %q", got, want)
		}
	}
}

// The realm's deletion reads what the identity provider answered. It used to
// print the code and exit 0 whatever it was, so a realm that could not be
// deleted was recorded as deleted. Retiring a tenant disables its realm, and
// that too may not fail quietly: a realm that could not be disabled is a
// tenant that was retired and can still sign in.
func TestTheRealmScriptsFailWhenTheRealmIsStillThere(t *testing.T) {
	scripts := map[string]string{
		"delete":             buildRealmDeleteScript("acme"),
		"disable":            buildRealmDisableScript("acme", "admin@acme.example", "gentian"),
		"disable, no broker": buildRealmDisableScript("acme", "admin@acme.example", ""),
	}
	for name, script := range scripts {
		if !strings.HasPrefix(script, "set -eu") {
			t.Errorf("%s: the script does not stop at the first failing command", name)
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "|| true") || strings.Contains(line, `|| echo`) {
				t.Errorf("%s: a failure is discarded: %s", name, line)
			}
		}
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v\n%s", name, err, out)
		}
	}
	for _, want := range []string{`204) echo "realm acme deleted"`, `404) echo "realm acme is not there"`, "exit 1"} {
		if !strings.Contains(scripts["delete"], want) {
			t.Errorf("the delete script is missing %q:\n%s", want, scripts["delete"])
		}
	}
	// With no kernel realm to broker through there is no second user to
	// disable, and the script must not ask an empty realm for one.
	if strings.Contains(scripts["disable, no broker"], "/admin/realms//") || strings.Contains(scripts["disable, no broker"], "USER_RESP") {
		t.Errorf("the disable script asks a realm with no name:\n%s", scripts["disable, no broker"])
	}
	if !strings.Contains(scripts["disable"], "/admin/realms/gentian/users?username=admin@acme.example") {
		t.Errorf("the disable script does not look for the administrator in the kernel realm:\n%s", scripts["disable"])
	}
}

// Realm resolution is one function: the controller's is the shared one.
func TestTheControllerResolvesTheRealmWhereEverythingElseDoes(t *testing.T) {
	named := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{KeycloakRealm: "corp"}}}
	plain := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	if keycloakRealmName(named) != "corp" || keycloak.RealmName(named) != "corp" {
		t.Error("a tenant that names its realm is not in it")
	}
	if keycloakRealmName(plain) != "acme" || keycloak.RealmName(plain) != "acme" {
		t.Error("a tenant that names no realm is not in the one called after it")
	}
}
