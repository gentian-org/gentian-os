/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"fmt"
	"os"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The label rule sets both labels on the pod template of each kind, and
// quotes them: a tenant may be named like a number.
func TestTheLabelRuleNamesTheAppAndTheTenantOnEveryPodTemplate(t *testing.T) {
	var doc struct {
		Patches []struct {
			Target struct{ Group, Version, Kind string }
			Patch  string
		}
	}
	if err := yaml.Unmarshal([]byte("patches:\n"+valuesPodLabelsPatch("wiki", "123")), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Patches) != len(valuesPodLabelKinds) {
		t.Fatalf("%d patches for %d kinds", len(doc.Patches), len(valuesPodLabelKinds))
	}
	for i, p := range doc.Patches {
		want := valuesPodLabelKinds[i]
		if p.Target.Group != want.group || p.Target.Kind != want.kind || p.Target.Version != "v1" {
			t.Errorf("patch %d targets %+v, want %+v", i, p.Target, want)
		}
		var obj struct {
			Kind string
			Spec struct {
				Template struct {
					Metadata struct{ Labels map[string]string }
				}
			}
		}
		if err := yaml.Unmarshal([]byte(p.Patch), &obj); err != nil {
			t.Fatalf("patch for %s: %v", want.kind, err)
		}
		labels := obj.Spec.Template.Metadata.Labels
		if obj.Kind != want.kind || labels["gentianos.io/app"] != "wiki" || labels["gentianos.io/tenant"] != "123" {
			t.Errorf("patch for %s sets %v on a %s", want.kind, labels, obj.Kind)
		}
	}
}

// Every release carries both rules, a placed chart's too: its values say
// nothing of the labels, and its pods carry them.
func TestEveryReleaseCarriesTheKeepRuleAndTheLabelRule(t *testing.T) {
	for _, in := range []*Input{{App: "admin-console", Placed: true}, {App: "wiki"}} {
		got := (&valuesRender{in: in, tenant: "acme"}).releasePatch()
		if !strings.HasPrefix(got, valuesKeepPatch) || !strings.Contains(got, fmt.Sprintf("gentianos.io/app: %q", in.App)) {
			t.Errorf("the patch of %s:\n%s", in.App, got)
		}
	}
}

// podTemplateKinds are the resources that carry a pod template, or are a
// pod, by the name a role gives them.
var podTemplateKinds = map[string]string{
	"deployments":            "Deployment",
	"statefulsets":           "StatefulSet",
	"daemonsets":             "DaemonSet",
	"replicasets":            "ReplicaSet",
	"jobs":                   "Job",
	"cronjobs":               "CronJob",
	"pods":                   "Pod",
	"replicationcontrollers": "ReplicationController",
}

// Every kind with a pod template that a tenant's chart may write is a kind
// the label rule covers. A kind added to the role is added to the rule.
func TestTheLabelRuleCoversEveryWorkloadKindAChartMayWrite(t *testing.T) {
	raw, err := os.ReadFile("../../crossplane/providers/provider-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, k := range valuesPodLabelKinds {
		covered[k.kind] = true
	}
	found := false
	for _, doc := range strings.Split(string(raw), "\n---") {
		role := &rbacv1.ClusterRole{}
		if err := yaml.Unmarshal([]byte(doc), role); err != nil || role.Kind != "ClusterRole" || role.Name != "gentian-tenant-apps-helm" {
			continue
		}
		found = true
		for _, rule := range role.Rules {
			writes := false
			for _, verb := range rule.Verbs {
				if verb == "create" || verb == "update" || verb == "patch" || verb == "*" {
					writes = true
				}
			}
			if !writes {
				continue
			}
			for _, resource := range rule.Resources {
				if resource == "*" {
					t.Errorf("the role writes every resource of %v", rule.APIGroups)
				}
				if kind, ok := podTemplateKinds[resource]; ok && !covered[kind] {
					t.Errorf("a tenant's chart may write %s, and the label rule has no patch for %s", resource, kind)
				}
			}
		}
	}
	if !found {
		t.Fatal("ClusterRole gentian-tenant-apps-helm not found in crossplane/providers/provider-rbac.yaml")
	}
}
