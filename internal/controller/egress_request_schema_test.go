/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"encoding/json"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	yamlv3 "sigs.k8s.io/yaml/goyaml.v3"
)

// An egress request names hosts or states a rule by address. A host is one
// name with an optional port, never a pattern or an address, and a profile
// names all its hosts in one request, which is what makes them one answer.
func TestTheProfileSchemaAdmitsAnEgressRequestByHostsOrByRule(t *testing.T) {
	t.Parallel()
	const reason = "reads the calendars of people who connected their account"
	hosts := func(name string, h ...any) map[string]any {
		return map[string]any{"name": name, "reason": reason, "hosts": h}
	}
	rule := map[string]any{"name": "relay", "reason": reason, "rule": map[string]any{
		"to":    []any{map[string]any{"ipBlock": map[string]any{"cidr": "203.0.113.0/24"}}},
		"ports": []any{map[string]any{"port": int64(587), "protocol": "TCP"}},
	}}
	for name, c := range map[string]struct {
		egress   []any
		admitted bool
	}{
		"hosts":                       {[]any{hosts("graph", "graph.microsoft.com", "login.microsoftonline.com")}, true},
		"a host with a port":          {[]any{hosts("api", "example.org:8443")}, true},
		"a rule":                      {[]any{rule}, true},
		"hosts and a rule, two names": {[]any{hosts("graph", "graph.microsoft.com"), rule}, true},
		"neither":                     {[]any{map[string]any{"name": "nothing", "reason": reason}}, false},
		"both in one request": {[]any{map[string]any{"name": "both", "reason": reason,
			"hosts": []any{"graph.microsoft.com"}, "rule": rule["rule"]}}, false},
		"no host":               {[]any{map[string]any{"name": "empty", "reason": reason, "hosts": []any{}}}, false},
		"hosts in two requests": {[]any{hosts("a", "a.example.org"), hosts("b", "b.example.org")}, false},
		"a pattern":             {[]any{hosts("all", "*.example.org")}, false},
		"an address":            {[]any{hosts("address", "203.0.113.7")}, false},
		"a name with no domain": {[]any{hosts("bare", "localhost")}, false},
		"upper case":            {[]any{hosts("upper", "Graph.Microsoft.com")}, false},
		"a scheme":              {[]any{hosts("scheme", "https://graph.microsoft.com")}, false},
		"a path":                {[]any{hosts("path", "graph.microsoft.com/v1.0")}, false},
		"port zero":             {[]any{hosts("zero", "example.org:0")}, false},
		"the same host twice":   {[]any{hosts("twice", "example.org", "example.org")}, false},
	} {
		var doc map[string]any
		if err := yamlv3.Unmarshal([]byte(digestProbeBundle), &doc); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		profile := &unstructured.Unstructured{}
		if err := profile.UnmarshalJSON(raw); err != nil {
			t.Fatal(err)
		}
		profile.SetName("egress-schema-probe")
		if err := unstructured.SetNestedSlice(profile.Object, c.egress, "spec", "requires", "privileges", "egress"); err != nil {
			t.Fatal(err)
		}
		err = testClient.Create(context.Background(), profile, client.DryRunAll)
		switch {
		case c.admitted && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case !c.admitted && err == nil:
			t.Errorf("%s: admitted", name)
		case !c.admitted && !k8serrors.IsInvalid(err):
			t.Errorf("%s: refused, but not by the schema: %v", name, err)
		}
	}
}
