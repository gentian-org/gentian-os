/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package dispatcher

import (
	"strings"
	"testing"
)

const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func step(name string) Runnable {
	return Runnable{
		Name: name, Image: "registry.example/steps/" + name, Digest: "sha256:" + hash,
		Command: []string{"/" + name}, Runtime: RuntimeContainer, DeadlineSeconds: 60,
	}
}

func table() Table {
	caller := Caller{Callback: Callback{Service: "flow", Namespace: "tenant-acme", Port: 80}, Concurrency: 2, Own: []Runnable{step("runner")}}
	caller.Tenant, caller.Component, caller.KeyHash = "acme", "flow", hash
	return Table{
		Callers: []Caller{caller},
		Tenants: []Tenant{{Tenant: "acme", Namespace: "tenant-acme-runs", Actions: []Runnable{step("ocr")}}},
	}
}

// A table the operator did not write is refused whole: a caller without a
// tenant, a key or a callback, and anything to run without a pinned image, a
// command, a deadline or a runtime the dispatcher has.
func TestATableThatIsNotOneIsRefused(t *testing.T) {
	good, err := table().Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTable([]byte(good)); err != nil {
		t.Fatalf("a table as the operator writes it: %v", err)
	}
	if empty, _ := (Table{}).Document(); empty != `{"callers":[],"tenants":[]}` {
		t.Fatalf("an empty table = %s", empty)
	}
	for name, change := range map[string]func(*Table){
		"a caller of no tenant in the table": func(t *Table) { t.Tenants = nil },
		"a caller without a key":             func(t *Table) { t.Callers[0].KeyHash = "" },
		"a caller without a callback":        func(t *Table) { t.Callers[0].Callback.Port = 0 },
		"a caller without a limit":           func(t *Table) { t.Callers[0].Concurrency = 0 },
		"two callers with one key":           func(t *Table) { t.Callers = append(t.Callers, t.Callers[0]) },
		"an action at a tag":                 func(t *Table) { t.Tenants[0].Actions[0].Digest = "latest" },
		"an action without a command":        func(t *Table) { t.Tenants[0].Actions[0].Command = nil },
		"an action without a deadline":       func(t *Table) { t.Tenants[0].Actions[0].DeadlineSeconds = 0 },
		"an action of another runtime":       func(t *Table) { t.Tenants[0].Actions[0].Runtime = "wasm" },
		"an action named twice":              func(t *Table) { t.Tenants[0].Actions = append(t.Tenants[0].Actions, step("ocr")) },
		"an own run at a tag":                func(t *Table) { t.Callers[0].Own[0].Digest = "" },
		"a tenant named twice":               func(t *Table) { t.Tenants = append(t.Tenants, t.Tenants[0]) },
		"a pull Secret that is not a name":   func(t *Table) { t.Tenants[0].Actions[0].PullSecrets = []string{"../other"} },
		"credentials that are not a name":    func(t *Table) { t.Tenants[0].Actions[0].Credentials = "../other" },
		"an own run with credentials":        func(t *Table) { t.Callers[0].Own[0].Credentials = "run-credentials-ocr" },
	} {
		changed := table()
		change(&changed)
		raw, _ := changed.Document()
		if _, err := ParseTable([]byte(raw)); err == nil || !strings.Contains(err.Error(), "the dispatcher's table") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A name means the caller's own run first, then the tenant's action.
func TestWhatANameMeansToACaller(t *testing.T) {
	tbl := table()
	tbl.Tenants[0].Actions = append(tbl.Tenants[0].Actions, step("runner"))
	caller := &tbl.Callers[0]
	if got, own, found := tbl.Runnable(caller, "runner"); !found || !own || got.Image != "registry.example/steps/runner" {
		t.Fatalf("runner = %+v own=%v found=%v", got, own, found)
	}
	if _, own, found := tbl.Runnable(caller, "ocr"); !found || own {
		t.Fatalf("ocr: own=%v found=%v", own, found)
	}
	if _, _, found := tbl.Runnable(caller, "nothing"); found {
		t.Fatal("a name nothing has was found")
	}
}

// A run of an installed action finds the hosts outside the action was
// granted in its host table. A caller's own run was granted none, whatever
// its entry says.
func TestARunOfAnActionHasItsGrantedHostsInItsHostTable(t *testing.T) {
	tenant := &Tenant{Tenant: "acme", Namespace: "tenant-acme-runs"}
	caller := &Caller{Callback: Callback{Service: "flow", Namespace: "tenant-acme", Port: 80}}
	caller.Tenant, caller.Component, caller.KeyHash = "acme", "flow", hash
	action := step("ocr")
	action.HostAliases = []HostAlias{{IP: "10.152.183.32", Hostname: "api.example.org"}, {IP: "10.152.183.40", Hostname: "auth.example.org"}}

	pod := Pod(tenant, caller, action, false, Start{ID: "run-1", Deadline: 60, Attempt: 1}, "registry.example/run:1")
	got := pod.Spec.HostAliases
	if len(got) != 2 || got[0].IP != "10.152.183.32" || len(got[0].Hostnames) != 1 || got[0].Hostnames[0] != "api.example.org" ||
		got[1].IP != "10.152.183.40" || got[1].Hostnames[0] != "auth.example.org" {
		t.Fatalf("the host table of an action's run: %+v", got)
	}
	if own := Pod(tenant, caller, action, true, Start{ID: "run-2", Deadline: 60, Attempt: 1}, "registry.example/run:1"); len(own.Spec.HostAliases) != 0 {
		t.Fatalf("a caller's own run has a host table: %+v", own.Spec.HostAliases)
	}
	if plain := Pod(tenant, caller, step("plain"), false, Start{ID: "run-3", Deadline: 60, Attempt: 1}, "registry.example/run:1"); len(plain.Spec.HostAliases) != 0 {
		t.Fatalf("an action with no granted host has a host table: %+v", plain.Spec.HostAliases)
	}
}
