/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

func definitionRequest(group string, definitions ...string) *gentianov1alpha1.APIExtensionRequest {
	req := &gentianov1alpha1.APIExtensionRequest{Name: "schedules", Group: group, ServiceAccount: "backup", Reason: "the schedules"}
	for _, d := range definitions {
		req.Definitions = append(req.Definitions, runtime.RawExtension{Raw: []byte(d)})
	}
	return req
}

func definition(name, group, plural, scope, extra string) string {
	return `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition",` +
		`"metadata":{"name":"` + name + `","labels":{"from":"the-profile"}},` +
		`"spec":{"group":"` + group + `","names":{"plural":"` + plural + `","kind":"Schedule"},"scope":"` + scope + `",` +
		`"versions":[{"name":"v1","served":true,"storage":true,"schema":{"openAPIV3Schema":{"type":"object"}}}]` + extra + `}}`
}

// What the platform installs of a profile's definitions: its own namespaced
// types, in its own group, with nothing of the profile's metadata, and no
// conversion webhook.
func TestOnlyAnAddOnsOwnNamespacedTypesAreInstalled(t *testing.T) {
	group := "backup.example.org"
	good := definition("schedules."+group, group, "schedules", "Namespaced", "")
	defs, err := security.APIExtensionDefinitions(definitionRequest(group, good))
	if err != nil {
		t.Fatalf("a good definition was refused: %v", err)
	}
	if len(defs) != 1 || len(defs[0].Labels) != 0 || defs[0].Name != "schedules."+group {
		t.Fatalf("installed as %+v; the profile's metadata is not carried", defs)
	}

	webhook := `,"conversion":{"strategy":"Webhook","webhook":{"conversionReviewVersions":["v1"],"clientConfig":{"url":"https://addon.example"}}}`
	refused := map[string]*gentianov1alpha1.APIExtensionRequest{
		"the platform's group":     definitionRequest("gentianos.io", definition("schedules.gentianos.io", "gentianos.io", "schedules", "Namespaced", "")),
		"a platform domain":        definitionRequest("x.gentianos.io", definition("schedules.x.gentianos.io", "x.gentianos.io", "schedules", "Namespaced", "")),
		"a Kubernetes group":       definitionRequest("apps.k8s.io", definition("schedules.apps.k8s.io", "apps.k8s.io", "schedules", "Namespaced", "")),
		"a group without a domain": definitionRequest("backup", definition("schedules.backup", "backup", "schedules", "Namespaced", "")),
		"another group":            definitionRequest(group, definition("schedules.other.example.org", "other.example.org", "schedules", "Namespaced", "")),
		"a name not its plural":    definitionRequest(group, definition("plans."+group, group, "schedules", "Namespaced", "")),
		"cluster scope":            definitionRequest(group, definition("schedules."+group, group, "schedules", "Cluster", "")),
		"a conversion webhook":     definitionRequest(group, definition("schedules."+group, group, "schedules", "Namespaced", webhook)),
		"the same type twice":      definitionRequest(group, good, good),
		"not a definition":         definitionRequest(group, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"}}`),
	}
	for name, req := range refused {
		if _, err := security.APIExtensionDefinitions(req); err == nil {
			t.Errorf("%s was installed", name)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s was refused without a word", name)
		}
	}
}
