/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// recorded is VerifyRecordedOnCluster on a cluster holding objects.
func recorded(t *testing.T, profile *gentianov1alpha1.ComponentProfile, objects ...client.Object) *Refusal {
	t.Helper()
	refusal, err := VerifyRecordedOnCluster(context.Background(), cluster(objects...), catalogueNamespace, profile)
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

// A profile placed from a catalogue records its build as the bundle beside
// it. One that is what the bundle says passes with no digest named anywhere
// else, and the digest answered for it is the bundle's.
func TestAProfileThatIsItsRecordedBuildIsVerified(t *testing.T) {
	p := held(t, wiki)
	p.Annotations[OriginAnnotation] = ClusterOrigin("main")
	digest, refusal := RecordedDigest(p)
	if refusal != nil || digest != Digest([]byte(wiki)) {
		t.Fatalf("recorded = %q, %+v; want the bundle's digest", digest, refusal)
	}
	if got := recorded(t, p); got != nil {
		t.Fatalf("a profile that is its recorded build was refused: %+v", got)
	}
}

// Changed and its bundle left as it was -- in git or in the cluster -- the
// profile is no longer the build recorded for it.
func TestAProfileThatIsNotItsRecordedBuildIsRefused(t *testing.T) {
	p := held(t, wiki)
	p.Spec.Package.Chart.Version = "6.6.6"
	refused(t, "a changed profile", recorded(t, p), ReasonMismatch, Short(Digest([]byte(wiki))), "as recorded", "spec.package")
}

// A companion the recorded bundle brings is held to it as well.
func TestACompanionOfARecordedBuildIsHeldToIt(t *testing.T) {
	p, objects := applied(t, shopBundle, ClusterOrigin("main"))
	if got := recorded(t, p, objects...); got != nil {
		t.Fatalf("a bundle that is all there was refused: %+v", got)
	}
	got := recorded(t, p)
	refused(t, "a missing companion", got, ReasonCompanionMissing)
	if !got.Retry {
		t.Fatal("a missing companion is not looked for again")
	}
	set(t, find(t, objects, KindConfigMap), "<html>other</html>", "data", "sso.html")
	refused(t, "a changed companion", recorded(t, p, objects...), ReasonCompanionMismatch)
}

// No record at all is a profile that came from no catalogue: the platform's
// chart ships it, and there is nothing to hold it to. A profile that says it
// came from a catalogue and shows no build is another matter, and so is a
// bundle that cannot be read.
func TestWhatHasNoRecordIsNotAskedAndAHalfRecordIsRefused(t *testing.T) {
	shipped := held(t, wiki)
	delete(shipped.Annotations, Annotation)
	if digest, refusal := RecordedDigest(shipped); digest != "" || refusal != nil {
		t.Fatalf("a profile with no record: %q, %+v", digest, refusal)
	}
	if got := recorded(t, shipped); got != nil {
		t.Fatalf("a profile with no record was refused: %+v", got)
	}

	stripped := held(t, wiki)
	delete(stripped.Annotations, Annotation)
	stripped.Annotations[OriginAnnotation] = ClusterOrigin("main")
	refused(t, "an origin with no bundle", recorded(t, stripped), ReasonUnverifiable, Annotation, "cluster/main")

	empty := held(t, wiki)
	empty.Annotations[Annotation] = " "
	refused(t, "an empty bundle", recorded(t, empty), ReasonUnverifiable)

	garbled := held(t, wiki)
	garbled.Annotations[Annotation] = "not base64 !"
	refused(t, "an unreadable bundle", recorded(t, garbled), ReasonUnverifiable)

	// Another profile's bundle, on this one.
	other := held(t, wiki)
	other.Name = "blog"
	refused(t, "another profile's bundle", recorded(t, other), ReasonMismatch, `"wiki"`)
}
