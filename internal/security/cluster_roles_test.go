/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/security"
)

const profileClusterRolesFile = "../../charts/gentian-os/templates/profile-cluster-roles.yaml"

// shippedClusterRoles are the ClusterRole objects of the chart's file, with
// the template's comment taken out.
func shippedClusterRoles(t *testing.T) []rbacv1.ClusterRole {
	t.Helper()
	raw, err := os.ReadFile(profileClusterRolesFile)
	if err != nil {
		t.Fatalf("read %s: %v", profileClusterRolesFile, err)
	}
	body := regexp.MustCompile(`(?s)\{\{-? */\*.*?\*/ *-?\}\}`).ReplaceAllString(string(raw), "")
	if strings.Contains(body, "{{") {
		t.Fatalf("%s is templated; the set is fixed, and this test reads it as it is written", profileClusterRolesFile)
	}
	var out []rbacv1.ClusterRole
	for _, doc := range strings.Split(body, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var role rbacv1.ClusterRole
		if err := yaml.UnmarshalStrict([]byte(doc), &role); err != nil {
			t.Fatalf("%s: %v", profileClusterRolesFile, err)
		}
		if role.Kind != "ClusterRole" {
			t.Fatalf("%s holds a %s; it holds the set's ClusterRoles and nothing else", profileClusterRolesFile, role.Kind)
		}
		out = append(out, role)
	}
	return out
}

// The names a profile may ask for and the roles the chart ships are one set,
// kept in two places.
func TestTheClusterRoleSetAndTheChartAgree(t *testing.T) {
	shipped := map[string]bool{}
	for _, role := range shippedClusterRoles(t) {
		name := role.Labels[security.ClusterRoleNameLabel]
		if name == "" || role.Name != security.ClusterRoleObjectName(name) {
			t.Errorf("ClusterRole %q: it must be named %s<name> and labeled %s: <name>",
				role.Name, security.ClusterRoleObjectPrefix, security.ClusterRoleNameLabel)
			continue
		}
		shipped[name] = true
		if _, ok := security.PlatformClusterRoles[name]; !ok {
			t.Errorf("the chart ships %q and PlatformClusterRoles does not name it", name)
		}
	}
	for name, role := range security.PlatformClusterRoles {
		if !shipped[name] {
			t.Errorf("PlatformClusterRoles names %q and the chart ships no such ClusterRole", name)
		}
		if strings.TrimSpace(role.What) == "" {
			t.Errorf("%q says nothing about what it allows; an approver reads that", name)
		}
	}
}

// A role of the set is narrow, and reads unless it is marked as one that
// writes. Anything broader is not added by editing the file: this fails, and
// the decision is the owner's.
func TestNoClusterRoleOfTheSetIsBroad(t *testing.T) {
	for _, role := range shippedClusterRoles(t) {
		if role.AggregationRule != nil {
			t.Errorf("%s aggregates other roles, so its rules are not the ones written here", role.Name)
		}
		writes := security.PlatformClusterRoles[role.Labels[security.ClusterRoleNameLabel]].Writes
		for _, problem := range broadRules(role.Rules, writes) {
			t.Errorf("%s: %s", role.Name, problem)
		}
	}
}

// A role that writes is break-glass's to approve; one that reads is the
// security officer's.
func TestARoleThatWritesIsApprovedByBreakGlass(t *testing.T) {
	for name, role := range security.PlatformClusterRoles {
		want := security.PrivilegeApproverSecurityOfficer
		if role.Writes {
			want = security.PrivilegeApproverBreakGlass
		}
		ref := security.PrivilegeRef(security.PrivilegeClusterRoles, name)
		if got := security.PrivilegeApprover(ref); got != want {
			t.Errorf("%s is approved by %q, want %q", ref, got, want)
		}
	}
}

// What "broad" means is itself held: each of these is refused.
func TestBroadRulesAreRecognised(t *testing.T) {
	read := []string{"get", "list", "watch"}
	broad := map[string]rbacv1.PolicyRule{
		"secrets":              {APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: read},
		"a write verb":         {APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "patch"}},
		"a wildcard verb":      {APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"*"}},
		"a wildcard resource":  {APIGroups: []string{""}, Resources: []string{"*"}, Verbs: read},
		"a wildcard group":     {APIGroups: []string{"*"}, Resources: []string{"nodes"}, Verbs: read},
		"rbac":                 {APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles"}, Verbs: read},
		"admission":            {APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"validatingwebhookconfigurations"}, Verbs: read},
		"nodes/proxy":          {APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, Verbs: read},
		"impersonation":        {APIGroups: []string{""}, Resources: []string{"users"}, Verbs: []string{"impersonate"}},
		"a non-resource URL":   {NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}},
		"no resource named":    {APIGroups: []string{""}, Verbs: read},
		"a wildcard in a name": {APIGroups: []string{""}, Resources: []string{"pods/*"}, Verbs: read},
	}
	for name, rule := range broad {
		if len(broadRules([]rbacv1.PolicyRule{rule}, false)) == 0 {
			t.Errorf("%s was taken as narrow", name)
		}
	}
	narrow := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: read}
	if problems := broadRules([]rbacv1.PolicyRule{narrow}, false); len(problems) != 0 {
		t.Errorf("reading nodes was refused: %v", problems)
	}

	// A role that writes is held as well: to the platform's own kinds, and
	// to no verb beyond the four.
	write := []string{"get", "create", "delete"}
	broadWrites := map[string]rbacv1.PolicyRule{
		"a core kind":       {APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: write},
		"another's kind":    {APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: write},
		"escalate":          {APIGroups: []string{"gentianos.io"}, Resources: []string{"tenantexports"}, Verbs: []string{"escalate"}},
		"deletecollection":  {APIGroups: []string{"gentianos.io"}, Resources: []string{"tenantexports"}, Verbs: []string{"deletecollection"}},
		"secrets":           {APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: write},
		"a platform policy": {APIGroups: []string{"gentianos.io"}, Resources: []string{"*"}, Verbs: write},
	}
	for name, rule := range broadWrites {
		if len(broadRules([]rbacv1.PolicyRule{rule}, true)) == 0 {
			t.Errorf("a role that writes: %s was taken as narrow", name)
		}
	}
	exports := rbacv1.PolicyRule{APIGroups: []string{"gentianos.io"}, Resources: []string{"tenantexports"}, Verbs: write}
	if problems := broadRules([]rbacv1.PolicyRule{exports}, true); len(problems) != 0 {
		t.Errorf("writing exports was refused: %v", problems)
	}
	if problems := broadRules([]rbacv1.PolicyRule{exports}, false); len(problems) == 0 {
		t.Error("a role not marked as one that writes was let write")
	}
}

func broadRules(rules []rbacv1.PolicyRule, writes bool) []string {
	var problems []string
	if len(rules) == 0 {
		problems = append(problems, "it has no rules")
	}
	for _, rule := range rules {
		if len(rule.NonResourceURLs) > 0 {
			problems = append(problems, "it names non-resource URLs")
		}
		if len(rule.Resources) == 0 || len(rule.APIGroups) == 0 {
			problems = append(problems, "a rule names no resource or no API group")
		}
		wrote := false
		for _, verb := range rule.Verbs {
			switch verb {
			case "get", "list", "watch":
			case "create", "update", "patch", "delete":
				wrote = true
				if !writes {
					problems = append(problems, "verb "+verb+" is not a read")
				}
			default:
				problems = append(problems, "verb "+verb+" is neither a read nor a write of one object")
			}
		}
		if wrote {
			for _, group := range rule.APIGroups {
				if group != "gentianos.io" {
					problems = append(problems, "it writes in "+group+", which is not the platform's")
				}
			}
		}
		for _, group := range rule.APIGroups {
			if strings.Contains(group, "*") {
				problems = append(problems, "a wildcard API group")
			}
			if group == "rbac.authorization.k8s.io" || group == "admissionregistration.k8s.io" {
				problems = append(problems, "it reaches "+group)
			}
		}
		for _, resource := range rule.Resources {
			switch {
			case strings.Contains(resource, "*"):
				problems = append(problems, "a wildcard resource")
			case resource == "secrets" || strings.HasPrefix(resource, "secrets/"):
				problems = append(problems, "it reads Secrets")
			case resource == "nodes/proxy":
				problems = append(problems, "it reaches nodes/proxy")
			}
		}
	}
	return problems
}
