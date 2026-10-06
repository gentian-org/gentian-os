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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	yamlv3 "sigs.k8s.io/yaml/goyaml.v3"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// A published profile that leaves to the schema everything the schema
// defaults, states a field the schema does not have, and writes values a
// YAML reader has to make something of.
const digestProbeBundle = `# Published by a catalogue source.
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: digest-probe
  labels:
    gentianos.io/family: probe
spec:
  classes: [app]
  launch: tile
  trustTier: certified
  version: "1.0.0"
  notInTheSchema: {dropped: true}
  package:
    chart:
      repository: oci://example.invalid/probe
      name: probe
      version: "1.0.0"
    extraValues:
      replicas: 2
      enabled: yes
      mode: 0777
      exact: 9007199254740993
      nested: {b: 1.0, a: [x, on, ~]}
  requires:
    services:
      database: {}
      cache: {}
      storage:
        s3: {}
  backup:
    quiesce: {}
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      subDomain: probe
      backend: {service: probe, port: 8080}
      tile:
        displayName: Probe
        logo: data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCI+PHJlY3Qgd2lkdGg9IjI0IiBoZWlnaHQ9IjI0Ii8+PC9zdmc+
        path: /
        relation: can_launch
`

// The check compares a bundle with the profile the API server serves, so it
// has to read the bundle into what the API server makes of it: defaults
// filled in, unknown fields gone. This asks a real API server rather than
// trusting that the two agree -- the document goes in the way Argo CD sends
// what kustomize built, comes back the way every reconciler reads it, and
// verifies. Then it is changed in the cluster, and does not.
func TestAProfileTheAPIServerStoredVerifiesAgainstItsBundle(t *testing.T) {
	ctx := context.Background()
	var doc map[string]any
	if err := yamlv3.Unmarshal([]byte(digestProbeBundle), &doc); err != nil {
		t.Fatal(err)
	}
	// Through JSON, as the API server receives it.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	applied := &unstructured.Unstructured{}
	if err := applied.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	applied.SetAnnotations(map[string]string{
		profilebundle.Annotation:         profilebundle.Encode([]byte(digestProbeBundle)),
		"argocd.argoproj.io/tracking-id": "gentian-catalogue-dev:gentianos.io/ComponentProfile:/digest-probe",
	})
	if err := testClient.Create(ctx, applied); err != nil {
		t.Fatalf("the API server refused the profile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), applied) })

	pinned := profilebundle.Digest([]byte(digestProbeBundle))
	stored := &gentianov1alpha1.ComponentProfile{}
	waitFor(t, envtestWaitTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "digest-probe"}, stored) == nil
	})
	// The premise: the server did default it, so the stored profile is not
	// the document's own bytes read plainly.
	if db := stored.Spec.Requires.Services.Database; db.Engine == "" || !db.DatabasePerTenant {
		t.Fatalf("the API server defaulted nothing: %+v", db)
	}
	if r := profilebundle.Verify(stored, pinned); r != nil {
		t.Fatalf("%s: %s", r.Reason, r.Message)
	}

	stored.Spec.Package.Chart.Version = "6.6.6"
	if err := testClient.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	changed := &gentianov1alpha1.ComponentProfile{}
	waitFor(t, envtestWaitTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "digest-probe"}, changed) == nil &&
			changed.Spec.Package.Chart.Version == "6.6.6"
	})
	if r := profilebundle.Verify(changed, pinned); r == nil || r.Reason != profilebundle.ReasonMismatch {
		t.Fatalf("a profile changed in the cluster verified: %+v", r)
	}
}
