/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// loadComponentProfileIndex lists cluster ComponentProfiles once per tenant
// reconcile, by name, and beside them every verified ProfileBuild as the
// profile it carries, by "<name>@<digest>": what a tenant pinned to a build
// that is not the profile's current one runs (appProfileForApp).
func loadComponentProfileIndex(ctx context.Context, c client.Client) (map[string]*gentianov1alpha1.ComponentProfile, error) {
	list := &gentianov1alpha1.ComponentProfileList{}
	if err := c.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list ComponentProfiles: %w", err)
	}
	index := make(map[string]*gentianov1alpha1.ComponentProfile, len(list.Items))
	for i := range list.Items {
		index[list.Items[i].Name] = &list.Items[i]
	}
	builds := &gentianov1alpha1.ProfileBuildList{}
	if err := c.List(ctx, builds); err != nil {
		if !meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("list ProfileBuilds: %w", err)
		}
		// A cluster whose definitions are older has no builds, and reads
		// every install from the profile as before.
		return index, nil
	}
	for i := range builds.Items {
		build := &builds.Items[i]
		profile, _, refusal := profilebundle.FromBuild(build)
		if refusal != nil {
			continue
		}
		index[buildKey(build.Spec.Profile, build.Spec.Digest)] = profile
	}
	return index, nil
}

func buildKey(name, digest string) string { return name + "@" + digest }

func appProfileFromIndex(index map[string]*gentianov1alpha1.ComponentProfile, name string) (*gentianov1alpha1.ComponentProfile, bool) {
	if index == nil || name == "" {
		return nil, false
	}
	profile, ok := index[name]
	return profile, ok
}

// appProfileFor is the profile a tenant's install runs: the build it is
// pinned to, when the cluster holds that build and the profile by name is
// another; otherwise the profile by name. digest is "" for an install
// pinned to nothing.
func appProfileFor(index map[string]*gentianov1alpha1.ComponentProfile, name, digest string) (*gentianov1alpha1.ComponentProfile, bool) {
	current, ok := appProfileFromIndex(index, name)
	if digest == "" || !ok {
		return current, ok
	}
	if recorded, refusal := profilebundle.RecordedDigest(current); refusal == nil && recorded == digest {
		return current, true
	}
	if build, found := index[buildKey(name, digest)]; found {
		return build, true
	}
	return current, true
}

// appProfileForApp is appProfileFor for an entry of a tenant's manifest.
func appProfileForApp(index map[string]*gentianov1alpha1.ComponentProfile, app gentianov1alpha1.TenantApp) (*gentianov1alpha1.ComponentProfile, bool) {
	return appProfileFor(index, app.Profile, app.Digest)
}

// getProfileFor reads the profile an install runs, by name and by the build
// it is pinned to: the ComponentProfile by name, unless the install's digest
// is another build than the one the profile carries and the cluster holds
// that build as a verified ProfileBuild, which is then what is read. A
// profile that is not on the cluster at all is still read from its build
// when there is one; otherwise the not-found error is the caller's, as it
// was. digest is "" for an install pinned to nothing.
func getProfileFor(ctx context.Context, c client.Reader, name, digest string) (*gentianov1alpha1.ComponentProfile, error) {
	profile := &gentianov1alpha1.ComponentProfile{}
	err := c.Get(ctx, client.ObjectKey{Name: name}, profile)
	if digest == "" {
		return profile, err
	}
	if err == nil {
		if recorded, refusal := profilebundle.RecordedDigest(profile); refusal == nil && recorded == digest {
			return profile, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	build := &gentianov1alpha1.ProfileBuild{}
	if berr := c.Get(ctx, client.ObjectKey{Name: profilebundle.BuildName(name, digest)}, build); berr != nil {
		if apierrors.IsNotFound(berr) || meta.IsNoMatchError(berr) {
			return profile, err
		}
		return nil, berr
	}
	fromBuild, _, refusal := profilebundle.FromBuild(build)
	if refusal != nil {
		return profile, err
	}
	return fromBuild, nil
}
