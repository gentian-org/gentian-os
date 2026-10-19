/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"encoding/base64"
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// BuildLabel is the label a ProfileBuild carries with its own name, so the
// app Composition can select the build a claim names by label, which is the
// one way it selects anything.
const BuildLabel = "gentianos.io/profile-build"

// BuildName is the name of the ProfileBuild of one build of a profile: the
// profile and the first twelve hex characters of the digest. Twelve tell two
// builds apart; a whole digest would not fit beside the name in a label.
func BuildName(profile, digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return profile + "-" + d
}

// FromBuild reads the profile a ProfileBuild carries, as the operator rolls
// it out for a tenant pinned to that build: the bundle decoded and held to
// the digest and to what a bundle may hold. Nil refusal means the profile is
// the build; what is returned then is the ComponentProfile as it would be on
// the cluster if this build were the profile's current one, defaults and
// annotations included, so everything that reads a profile reads this one
// the same way. The bundle's bytes come back too.
func FromBuild(build *gentianov1alpha1.ProfileBuild) (*gentianov1alpha1.ComponentProfile, []byte, *Refusal) {
	bundle, err := base64.StdEncoding.DecodeString(build.Spec.Bundle)
	if err != nil {
		return nil, nil, &Refusal{Reason: ReasonUnverifiable,
			Message: fmt.Sprintf("ProfileBuild %s carries a bundle that does not decode: %v", build.Name, err)}
	}
	if got := Digest(bundle); got != build.Spec.Digest {
		return nil, nil, &Refusal{Reason: ReasonMismatch,
			Message: fmt.Sprintf("ProfileBuild %s says it is %s and its bundle hashes to %s", build.Name, Short(build.Spec.Digest), Short(got))}
	}
	if build.Name != BuildName(build.Spec.Profile, build.Spec.Digest) {
		return nil, nil, &Refusal{Reason: ReasonMismatch,
			Message: fmt.Sprintf("ProfileBuild %s is not named for its profile %s and digest %s", build.Name, build.Spec.Profile, Short(build.Spec.Digest))}
	}
	read, err := Check(bundle, build.Spec.Profile, build.Spec.Origin)
	if err != nil {
		return nil, nil, &Refusal{Reason: ReasonRefused,
			Message: fmt.Sprintf("ProfileBuild %s is the pinned build and %v", build.Name, err)}
	}
	profile, err := decode(read.Profile)
	if err != nil {
		return nil, nil, &Refusal{Reason: ReasonUnverifiable,
			Message: fmt.Sprintf("ProfileBuild %s: its profile cannot be read as a ComponentProfile: %v", build.Name, err)}
	}
	if profile.Annotations == nil {
		profile.Annotations = map[string]string{}
	}
	profile.Annotations[Annotation] = build.Spec.Bundle
	profile.Annotations[OriginAnnotation] = build.Spec.Origin
	return profile, bundle, nil
}
