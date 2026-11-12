/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta/metatest"
)

// What every pod the operator builds runs under, held to
// testdata/security-contexts.txt.
func TestSecurityContextTranscript(t *testing.T) {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace = "wiki", "tenant-demo"
	s := &signInSidecar{service: "wiki-sign-in", host: "wiki.example", handlerDigest: "d"}
	objs := map[string]any{
		"psql":      psqlContainer("c", "true"),
		"cache":     cacheContainer("c", "u", "p", "true"),
		"mariadb":   mariadbContainer("c", "true"),
		"keycloak":  keycloakContainer("c", "true"),
		"perimeter": perimeterDeployment("p", "dmz", map[string]string{"a": "b"}, "h"),
		"sign-in":   signInDeployment(comp, s, map[string]string{"a": "b"}, true, pullSecrets{images: []string{"r"}}),
	}
	for i, obj := range arbiterObjects("image", "ctl", "caller") {
		objs["arbiter "+string(rune('a'+i))] = obj
	}
	for i, obj := range turnEdgeObjects("image", []byte("conf")) {
		objs["turn "+string(rune('a'+i))] = obj
	}
	metatest.Hold(t, "testdata/security-contexts.txt", metatest.Objects(t, objs))
}
