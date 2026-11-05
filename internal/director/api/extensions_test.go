/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// extendedOperator is the operator of a cluster where one add-on type is
// installed and granted: schedules.backup.example.org, whose spec takes a
// cron schedule and a number of bundles to keep.
type extendedOperator struct {
	importingOperator
}

const scheduleSchema = `{"type":"object","properties":{"spec":{"type":"object","required":["schedule"],` +
	`"properties":{"schedule":{"type":"string"},"keep":{"type":"integer","minimum":1}},"additionalProperties":false}}}`

func (o *extendedOperator) Get(ctx context.Context, path string, q url.Values) (int, []byte, error) {
	if path == "/v1/api-extensions" {
		return http.StatusOK, []byte(`{"types":[{"group":"backup.example.org","resource":"schedules","kind":"Schedule",` +
			`"version":"v1","profile":"backup","schema":` + scheduleSchema + `}]}`), nil
	}
	return o.importingOperator.Get(ctx, path, q)
}

// A tenant's administrator declares an object of an add-on's type as a file
// of the tenant's directory, in the tenant's namespace; the cluster's
// administrator declares the cluster's in the platform tenant's. An object of
// a type nobody granted, or that its type would refuse, is not committed.
func TestAnAddOnsObjectsAreDeclaredByPeopleInGit(t *testing.T) {
	h := startWith(t, &extendedOperator{})
	adoptKernelRealm(t, h, "other")
	tom := h.token(t, "tenant-demo", "tom")
	alice := h.token(t, "gentian", "alice")
	path := "/v1/tenants/demo/extensions/backup.example.org/schedules/nightly"
	nightly := `{"spec":{"schedule":"0 2 * * *","keep":7}}`

	code, body := h.do(t, "PUT", path, tom, nightly)
	if code != http.StatusAccepted {
		t.Fatalf("tom's schedule = %d %v", code, body)
	}
	file := h.remoteFile(t, "demo", "ext-schedules.backup.example.org-nightly.yaml")
	for _, want := range []string{"apiVersion: backup.example.org/v1", "kind: Schedule", "name: nightly", "namespace: tenant-demo", "schedule: 0 2 * * *", "keep: 7"} {
		if !strings.Contains(file, want) {
			t.Errorf("the declared object lacks %q:\n%s", want, file)
		}
	}
	if !strings.Contains(h.remoteFile(t, "demo", "kustomization.yaml"), "ext-schedules.backup.example.org-nightly.yaml") {
		t.Error("the tenant's kustomization does not list the object")
	}

	before := h.tip(t)
	refused := map[string]struct {
		path, token, body string
		code              int
	}{
		"another tenant's":                           {"/v1/tenants/solo/extensions/backup.example.org/schedules/nightly", tom, nightly, http.StatusForbidden},
		"a type not granted":                         {"/v1/tenants/demo/extensions/other.example.org/things/x", tom, `{"spec":{}}`, http.StatusNotFound},
		"a platform type":                            {"/v1/tenants/demo/extensions/gentianos.io/backuppolicies/x", tom, `{"spec":{}}`, http.StatusNotFound},
		"what its type refuses":                      {path, tom, `{"spec":{"schedule":"0 2 * * *","keep":0}}`, http.StatusBadRequest},
		"a field it lacks":                           {path, tom, `{"spec":{"schedule":"0 2 * * *","target":"https://elsewhere"}}`, http.StatusBadRequest},
		"a name with a dot":                          {"/v1/tenants/demo/extensions/backup.example.org/schedules/a.b", tom, nightly, http.StatusBadRequest},
		"the cluster's, by a tenant's administrator": {"/v1/clusters/" + dt.Cluster + "/extensions/backup.example.org/schedules/all", tom, nightly, http.StatusForbidden},
	}
	for name, tc := range refused {
		if code, body := h.do(t, "PUT", tc.path, tc.token, tc.body); code != tc.code {
			t.Errorf("%s = %d %v, want %d", name, code, body, tc.code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused object was committed")
	}

	// The cluster's goes to the platform tenant.
	if code, body := h.do(t, "PUT", "/v1/clusters/"+dt.Cluster+"/extensions/backup.example.org/schedules/all", alice, nightly); code != http.StatusAccepted {
		t.Fatalf("alice's schedule for the cluster = %d %v", code, body)
	}
	if file := h.remoteFile(t, "other", "ext-schedules.backup.example.org-all.yaml"); !strings.Contains(file, "namespace: tenant-other") {
		t.Errorf("the cluster's object is not the platform tenant's:\n%s", file)
	}

	// Removed by the same relation, and not twice.
	if code, body := h.do(t, "DELETE", path, tom, ""); code != http.StatusAccepted {
		t.Fatalf("tom's removal = %d %v", code, body)
	}
	if h.remoteFile(t, "demo", "ext-schedules.backup.example.org-nightly.yaml") != "" {
		t.Error("the file is still there")
	}
	if code, _ := h.do(t, "DELETE", path, tom, ""); code != http.StatusNotFound {
		t.Errorf("removing it twice = %d", code)
	}
}
