/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

const tenantBefore = `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  displayName: Demo
  apps:
  - profile: nextcloud
  - profile: element
    addons: [bridge]
    addonPins:
    - name: bridge
      digest: sha256:aaaa
`

func changes(t *testing.T, before, after string) []schemacheck.Change {
	t.Helper()
	var was []byte
	if before != "" {
		was = []byte(before)
	}
	got, err := schemacheck.Embedded().Changes(was, []byte(after))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWhatAWriteSetsIsReadOffTheManifest(t *testing.T) {
	cases := []struct {
		name          string
		before, after string
		want          []schemacheck.Change
	}{
		{
			name:   "a new tenant sets everything in it",
			before: "",
			after:  "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  displayName: Demo\n  apps:\n  - profile: nextcloud\n",
			want:   []schemacheck.Change{{Kind: "Tenant", Fields: []string{"spec.apps[].profile", "spec.displayName"}}},
		},
		{
			name:   "nothing changed sets nothing",
			before: tenantBefore,
			after:  tenantBefore + "# a comment\n",
		},
		{
			name:   "an app installed at a digest sets the digest",
			before: tenantBefore,
			after:  tenantBefore + "  - profile: wiki\n    digest: sha256:bbbb\n",
			want:   []schemacheck.Change{{Kind: "Tenant", Fields: []string{"spec.apps[].digest", "spec.apps[].profile"}}},
		},
		{
			name:   "removing the first app sets nothing, though every later entry moved up",
			before: tenantBefore,
			after:  strings.Replace(tenantBefore, "  - profile: nextcloud\n", "", 1),
		},
		{
			name:   "removing the last entry of a list sets nothing",
			before: "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  apps:\n  - profile: nextcloud\n",
			after:  "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  apps: []\n",
		},
		{
			name:   "removing a pin sets nothing",
			before: tenantBefore,
			after:  strings.Replace(tenantBefore, "    addonPins:\n    - name: bridge\n      digest: sha256:aaaa\n", "", 1),
		},
		{
			name:   "a pin moved to another build sets the pin",
			before: tenantBefore,
			after:  strings.Replace(tenantBefore, "sha256:aaaa", "sha256:cccc", 1),
			want:   []schemacheck.Change{{Kind: "Tenant", Fields: []string{"spec.apps[].addonPins[].digest"}}},
		},
		{
			name:   "a second entry holding a value another already holds is still a value set",
			before: tenantBefore,
			after:  tenantBefore + "  - profile: nextcloud\n",
			want:   []schemacheck.Change{{Kind: "Tenant", Fields: []string{"spec.apps[].profile"}}},
		},
		{
			name:   "a kustomize patch is a manifest like any other",
			before: "",
			after:  "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  locales: [de, fr]\n",
			want:   []schemacheck.Change{{Kind: "Tenant", Fields: []string{"spec.locales[]"}}},
		},
		{
			name:   "a kind Crossplane generates is read against its XRD",
			before: "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  kernelDomain: k.example\n",
			after:  "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  kernelDomain: k.example\n  tenancyMode: multi\n",
			want:   []schemacheck.Change{{Kind: "Cluster", Fields: []string{"spec.tenancyMode"}}},
		},
		{
			name:   "metadata is never pruned, so it is not a field set",
			before: tenantBefore,
			after:  strings.Replace(tenantBefore, "  name: demo\n", "  name: demo\n  annotations:\n    gentianos.io/x: y\n", 1),
		},
		{
			name:   "a manifest of another API group is not this check's",
			before: "",
			after:  "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- tenant.yaml\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := changes(t, c.before, c.after)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// report builds what an operator with this binary's definitions answers,
// with the states given for the kinds named and ok for the rest.
func report(states map[string]schemacheck.KindReport) schemacheck.Report {
	set := schemacheck.Embedded()
	r := schemacheck.Report{Checked: true, OK: len(states) == 0, Definitions: set.Digest}
	for _, def := range set.Definitions {
		k := schemacheck.KindReport{Kind: def.Kind, CRD: def.CRD, Source: def.Source, XRD: def.XRD, State: schemacheck.StateOK}
		if s, ok := states[def.Kind]; ok {
			k.State, k.Missing, k.Detail, k.Remedy = s.State, s.Missing, s.Detail, def.Remedy()
		}
		r.Kinds = append(r.Kinds, k)
	}
	return r
}

func answer(r schemacheck.Report) schemacheck.Source {
	return func() (schemacheck.Report, error) { return r, nil }
}

func TestAWriteIsRefusedOnlyForAFieldTheClusterWouldDrop(t *testing.T) {
	set := schemacheck.Embedded()
	outdated := answer(report(map[string]schemacheck.KindReport{
		"Tenant": {State: schemacheck.StateOutdated, Missing: []string{"spec.apps[].addonPins"}},
	}))
	pin := changes(t, tenantBefore, strings.Replace(tenantBefore, "sha256:aaaa", "sha256:cccc", 1))
	install := changes(t, tenantBefore, tenantBefore+"  - profile: wiki\n")
	uninstall := changes(t, tenantBefore, strings.Replace(tenantBefore, "  - profile: nextcloud\n", "", 1))
	cluster := changes(t, "", "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  tenancyMode: multi\n")

	refusal := schemacheck.Decide(set, outdated, pin)
	if refusal == nil || refusal.Unconfirmed {
		t.Fatalf("a pin written to a cluster that drops pins must be refused as outdated, got %+v", refusal)
	}
	if refusal.Kind != "Tenant" || !reflect.DeepEqual(refusal.Fields, []string{"spec.apps[].addonPins[].digest"}) {
		t.Errorf("the refusal names %s %v", refusal.Kind, refusal.Fields)
	}
	for _, want := range []string{"Tenant", "tenants.gentianos.io", "spec.apps[].addonPins[].digest", "sync the gentian-os Application"} {
		if !strings.Contains(refusal.Message, want) {
			t.Errorf("the refusal does not name %q: %s", want, refusal.Message)
		}
	}
	for name, c := range map[string][]schemacheck.Change{"an install without a pin": install, "an uninstall": uninstall, "a write to another kind": cluster} {
		if r := schemacheck.Decide(set, outdated, c); r != nil {
			t.Errorf("%s does not need the missing field and was refused: %s", name, r.Message)
		}
	}

	// A kind Crossplane generates names the installer step, not the chart.
	xrd := answer(report(map[string]schemacheck.KindReport{
		"Cluster": {State: schemacheck.StateOutdated, Missing: []string{"spec.tenancyMode"}},
	}))
	refusal = schemacheck.Decide(set, xrd, cluster)
	if refusal == nil || !strings.Contains(refusal.Message, "./install.sh --only B-06") {
		t.Fatalf("a Cluster claim field the cluster drops must be refused naming step B-06, got %+v", refusal)
	}

	// A kind the cluster does not have fails loudly on apply; it is not held
	// back here.
	absent := answer(report(map[string]schemacheck.KindReport{"Cluster": {State: schemacheck.StateAbsent}}))
	if r := schemacheck.Decide(set, absent, cluster); r != nil {
		t.Errorf("a write to an absent kind was refused: %s", r.Message)
	}
}

func TestAWriteIsRefusedWhenTheStateCannotBeConfirmed(t *testing.T) {
	set := schemacheck.Embedded()
	install := changes(t, tenantBefore, tenantBefore+"  - profile: wiki\n")
	uninstall := changes(t, tenantBefore, strings.Replace(tenantBefore, "  - profile: nextcloud\n", "", 1))

	otherBuild := report(nil)
	otherBuild.Definitions = "sha256:another"
	for name, source := range map[string]schemacheck.Source{
		"no operator to ask":         nil,
		"the operator unreachable":   func() (schemacheck.Report, error) { return schemacheck.Report{}, errors.New("connection refused") },
		"the operator not yet done":  answer(schemacheck.Report{Definitions: set.Digest}),
		"an operator of other build": answer(otherBuild),
		"the definition unreadable":  answer(report(map[string]schemacheck.KindReport{"Tenant": {State: schemacheck.StateUnreadable, Detail: "forbidden"}})),
	} {
		refusal := schemacheck.Decide(set, source, install)
		if refusal == nil || !refusal.Unconfirmed {
			t.Errorf("%s: a write that sets a field must be refused as unconfirmed, got %+v", name, refusal)
			continue
		}
		if !strings.Contains(refusal.Message, "could not be confirmed") {
			t.Errorf("%s: the refusal does not say the state could not be confirmed: %s", name, refusal.Message)
		}
		// A write that sets nothing has nothing to lose and is served.
		if r := schemacheck.Decide(set, source, uninstall); r != nil {
			t.Errorf("%s: a removal was refused: %s", name, r.Message)
		}
	}
}

func TestAnAnswerIsRememberedBrieflyAndAFailureNotAtAll(t *testing.T) {
	now := time.Unix(1000, 0)
	asked := 0
	var fail error
	cached := schemacheck.Cached(func() (schemacheck.Report, error) {
		asked++
		return schemacheck.Report{Checked: true}, fail
	}, 15*time.Second, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if _, err := cached(); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times within the window, want 1", asked)
	}
	now = now.Add(16 * time.Second)
	fail = errors.New("down")
	for i := 0; i < 2; i++ {
		if _, err := cached(); err == nil {
			t.Fatal("a failure was answered from memory")
		}
	}
	if asked != 3 {
		t.Fatalf("asked %d times, want 3: a failure is asked again every time", asked)
	}
}
