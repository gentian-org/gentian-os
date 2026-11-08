/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const buildWiki = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: wiki
  labels:
    gentianos.io/profile-name: wiki
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/wiki
      name: wiki
      version: "1.0.0"
`

func buildOf(t *testing.T, bundle string) *gentianov1alpha1.ProfileBuild {
	t.Helper()
	p := &gentianov1alpha1.ComponentProfile{}
	if err := yaml.Unmarshal([]byte(bundle), p); err != nil {
		t.Fatal(err)
	}
	digest := Digest([]byte(bundle))
	_ = p
	return &gentianov1alpha1.ProfileBuild{
		ObjectMeta: metav1.ObjectMeta{Name: BuildName("wiki", digest)},
		Spec: gentianov1alpha1.ProfileBuildSpec{Profile: "wiki", Digest: digest, Origin: "cluster/gentian",
			Bundle: Encode([]byte(bundle))},
	}
}

func TestABuildNameIsTheProfileAndTwelveHexOfTheDigest(t *testing.T) {
	if got := BuildName("wiki", "sha256:0123456789abcdef0123456789abcdef"); got != "wiki-0123456789ab" {
		t.Fatalf("BuildName = %q", got)
	}
}

func TestTheProfileOfABuildIsReadWithItsBundleAndOrigin(t *testing.T) {
	profile, bundle, refusal := FromBuild(buildOf(t, buildWiki))
	if refusal != nil {
		t.Fatalf("refused: %s %s", refusal.Reason, refusal.Message)
	}
	if profile.Name != "wiki" || profile.Spec.Package.Chart.Version != "1.0.0" || string(bundle) != buildWiki {
		t.Fatalf("profile %s at %s, bundle %q", profile.Name, profile.Spec.Package.Chart.Version, bundle)
	}
	if profile.Annotations[Annotation] != Encode([]byte(buildWiki)) || profile.Annotations[OriginAnnotation] != "cluster/gentian" {
		t.Fatalf("annotations = %v", profile.Annotations)
	}
	// What it carries verifies as the profile on the cluster would.
	if r := Verify(profile, Digest([]byte(buildWiki))); r != nil {
		t.Fatalf("the build's profile does not verify against its own digest: %s", r.Message)
	}
}

func TestABuildThatIsNotWhatItSaysIsRefused(t *testing.T) {
	other := strings.ReplaceAll(buildWiki, `version: "1.0.0"`, `version: "2.0.0"`)

	tampered := buildOf(t, buildWiki)
	tampered.Spec.Bundle = Encode([]byte(other))
	if _, _, r := FromBuild(tampered); r == nil || r.Reason != ReasonMismatch {
		t.Fatalf("a bundle of another digest: %+v", r)
	}

	misnamed := buildOf(t, buildWiki)
	misnamed.Name = "wiki-000000000000"
	if _, _, r := FromBuild(misnamed); r == nil || r.Reason != ReasonMismatch {
		t.Fatalf("a build not named for its digest: %+v", r)
	}

	garbage := buildOf(t, buildWiki)
	garbage.Spec.Bundle = "not base64!"
	if _, _, r := FromBuild(garbage); r == nil || r.Reason != ReasonUnverifiable {
		t.Fatalf("a bundle that does not decode: %+v", r)
	}
}
