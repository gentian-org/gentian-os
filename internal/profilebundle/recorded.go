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
	"encoding/base64"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A Component nobody pinned.
//
// A Component the platform places itself -- of a profile that declares it a
// default -- comes from no install, so it names no digest. The profile may
// still be one that was placed at a digest: the installer places a default
// profile only at a stated one, and what it records is what the director
// records for an install, the bundle beside the profile (Annotation) and the
// catalogue it came from (OriginAnnotation). The digest recorded for such a
// profile is the digest of that bundle, and the profile and every companion
// are held to it at rollout as a pinned install's are.
//
// What this notices is a profile, or a companion, that is no longer what the
// recorded bundle says: edited in git without its bundle, edited in the
// cluster, or left over from another build. What it cannot notice is a
// profile replaced together with its bundle. A pin is stated somewhere else
// than on the profile, and a default has none.

// RecordedDigest answers the digest recorded for a profile: that of the
// bundle it carries. Empty, and no refusal, for a profile with no record at
// all -- one the platform's chart ships, which came from no catalogue. A
// profile that states a catalogue origin and carries no bundle that can be
// read is refused: it says it was placed from a catalogue, and nothing shows
// at which build.
func RecordedDigest(profile *gentianov1alpha1.ComponentProfile) (string, *Refusal) {
	encoded, carries := profile.Annotations[Annotation]
	encoded = strings.TrimSpace(encoded)
	origin := strings.TrimSpace(profile.Annotations[OriginAnnotation])
	if !carries && origin == "" {
		return "", nil
	}
	if encoded == "" {
		return "", &Refusal{Reason: ReasonUnverifiable, Message: fmt.Sprintf(
			"ComponentProfile %q was placed from a catalogue (%s) and carries no bundle to check it against (annotation %s)",
			profile.Name, origin, Annotation)}
	}
	bundle, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", &Refusal{Reason: ReasonUnverifiable, Message: fmt.Sprintf(
			"the bundle ComponentProfile %q carries cannot be read: %v", profile.Name, err)}
	}
	return Digest(bundle), nil
}

// VerifyRecordedOnCluster is VerifyOnCluster for a Component that names no
// digest: the profile and its companions are held to the digest recorded for
// the profile. Nil for a profile with no record.
func VerifyRecordedOnCluster(
	ctx context.Context, c client.Reader, namespace string, profile *gentianov1alpha1.ComponentProfile,
) (*Refusal, error) {
	digest, refusal := RecordedDigest(profile)
	if refusal != nil || digest == "" {
		return refusal, nil
	}
	if refusal := verify(profile, digest, "as recorded"); refusal != nil {
		return refusal, nil
	}
	return VerifyCompanions(ctx, c, namespace, profile)
}
