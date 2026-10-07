/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The companions in the cluster, against the bundle.
//
// A pinned install is pinned to the whole bundle, so the profile being the
// pinned build is half of it. Each object the bundle brings beside the
// profile is read from the cluster and compared with what the bytes say, and
// one that is missing or says something else holds the rollout exactly as a
// wrong profile does.
//
// How closely one can be compared depends on what the operator knows of its
// kind:
//
//   - A ConfigMap is its data, and the data has to be the bundle's exactly:
//     no key more, none less, none changed.
//   - An OIDCPackCatalog and a Customization are this operator's own kinds.
//     Both sides are read through the type the operator reads them with, as
//     the profile is, and have to be equal. Neither schema states a default,
//     so nothing may be in the cluster that the bundle does not say (a test
//     holds the schemas to that).
//   - A Composition is Crossplane's kind, and the operator carries no schema
//     for it, so it cannot tell a default the API server filled in from a
//     field somebody added. Everything the bundle states has to be in the
//     cluster unchanged, and a list has to have the bundle's length -- a
//     pipeline step can be neither added nor removed. What every step is
//     given to do, its `input`, is free-form and never defaulted, and has to
//     be the bundle's exactly. What is NOT noticed is a field added beside
//     the ones the bundle states, outside an input: `spec.environment`, say,
//     or a step's `credentials`.
//
// Labels are the platform's handle on a companion -- a Composition finds
// what came with a profile by them -- so those the bundle states have to be
// there, and no other gentianos.io label may be.

// VerifyOnCluster is Verify and then VerifyCompanions: whether what the
// cluster holds under a profile's name is, all of it, the build digest names.
//
// namespace is where the catalogue is applied: the namespace of the
// companions that have one.
func VerifyOnCluster(
	ctx context.Context, c client.Reader, namespace string, profile *gentianov1alpha1.ComponentProfile, digest string,
) (*Refusal, error) {
	if refusal := Verify(profile, digest); refusal != nil {
		return refusal, nil
	}
	return VerifyCompanions(ctx, c, namespace, profile)
}

// OwnComposition is the Composition a profile's bundle brings for it, or ""
// when it brings none. For a profile Verify has passed: the bundle is then
// known to be the pinned build and to hold only this profile's own.
func OwnComposition(profile *gentianov1alpha1.ComponentProfile) string {
	bundle, ok := carried(profile)
	if !ok {
		return ""
	}
	read, err := Check(bundle, profile.Name, profile.Annotations[OriginAnnotation])
	if err != nil {
		return ""
	}
	return read.Composition()
}

// carried is the bundle a profile carries, decoded.
func carried(profile *gentianov1alpha1.ComponentProfile) ([]byte, bool) {
	encoded := strings.TrimSpace(profile.Annotations[Annotation])
	if encoded == "" {
		return nil, false
	}
	bundle, err := base64.StdEncoding.DecodeString(encoded)
	return bundle, err == nil
}

// VerifyCompanions reports whether every companion the profile's bundle
// brings is in the cluster as the bundle says. Nil means each is. It is
// called for a profile Verify has passed, and reads the bundle Verify hashed.
//
// A refusal here may end on its own -- Argo CD applies the profile and its
// companions in one sync but not in one instant -- so it asks to be looked
// at again.
func VerifyCompanions(
	ctx context.Context, c client.Reader, namespace string, profile *gentianov1alpha1.ComponentProfile,
) (*Refusal, error) {
	bundle, ok := carried(profile)
	if !ok {
		return nil, nil
	}
	read, err := Check(bundle, profile.Name, profile.Annotations[OriginAnnotation])
	if err != nil {
		return &Refusal{Reason: ReasonRefused, Message: fmt.Sprintf(
			"the bundle of ComponentProfile %q is not one that is rolled out: %v", profile.Name, err)}, nil
	}
	for _, companion := range read.Companions {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(runtimeschema.FromAPIVersionAndKind(companion.APIVersion, companion.Kind))
		key := types.NamespacedName{Name: companion.Name}
		where := ""
		if companion.Namespaced {
			key.Namespace = namespace
			where = " in " + namespace
		}
		if err := c.Get(ctx, key, live); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("reading the %s of ComponentProfile %s: %w", companion, profile.Name, err)
			}
			return &Refusal{Reason: ReasonCompanionMissing, Retry: true, Message: fmt.Sprintf(
				"the bundle of ComponentProfile %q brings the %s, and it is not on this cluster%s",
				profile.Name, companion, where)}, nil
		}
		differs, err := companionDiffers(companion, live.Object)
		if err != nil {
			return &Refusal{Reason: ReasonUnverifiable, Message: fmt.Sprintf(
				"the %s of ComponentProfile %q could not be compared with the bundle: %v", companion, profile.Name, err)}, nil
		}
		if differs != "" {
			return &Refusal{Reason: ReasonCompanionMismatch, Retry: true, Message: fmt.Sprintf(
				"the bundle of ComponentProfile %q brings the %s, and the one on this cluster%s is not what it says: %s differs",
				profile.Name, companion, where, differs)}, nil
		}
	}
	return nil, nil
}

// companionDiffers names the first place a companion in the cluster is not
// what the bundle says, or "" when it is.
func companionDiffers(said Companion, live map[string]any) (string, error) {
	if key := labelsDiffer(said.Object, live); key != "" {
		return "label " + key, nil
	}
	switch said.Kind {
	case KindConfigMap:
		if binary, _ := live["binaryData"].(map[string]any); len(binary) > 0 {
			return "binaryData", nil
		}
		return exact(orEmpty(said.Object["data"]), orEmpty(live["data"]), "data")
	case KindOIDCPacks:
		return typed[gentianov1alpha1.OIDCPackCatalogSpec](said.Object["spec"], live["spec"])
	case KindCustomization:
		return typed[gentianov1alpha1.CustomizationSpec](said.Object["spec"], live["spec"])
	case KindComposition:
		return stated(said.Object["spec"], live["spec"], "spec")
	}
	return "", fmt.Errorf("a %s is not a kind that is compared", said.Kind)
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// labelsDiffer names a label the bundle states that the object does not have
// so, or one of the platform's it has without the bundle stating it.
func labelsDiffer(said, live map[string]any) string {
	labels := func(obj map[string]any) map[string]any {
		meta, _ := obj["metadata"].(map[string]any)
		out, _ := meta["labels"].(map[string]any)
		return out
	}
	want, have := labels(said), labels(live)
	var keys []string
	for key, value := range want {
		if have[key] != value {
			keys = append(keys, key)
		}
	}
	for key := range have {
		if _, ok := want[key]; !ok && strings.HasPrefix(key, ownedPrefix) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}

// typed compares two specs read through the type the operator reads the
// kind with: a field the type does not have is dropped on both sides, as the
// API server drops it, and everything else has to be equal.
func typed[T any](said, live any) (string, error) {
	through := func(v any) (map[string][]byte, error) {
		raw, err := json.Marshal(orEmpty(v))
		if err != nil {
			return nil, err
		}
		var spec T
		if err := json.Unmarshal(raw, &spec); err != nil {
			return nil, err
		}
		return canonical(&spec)
	}
	left, err := through(said)
	if err != nil {
		return "", fmt.Errorf("the bundle's: %w", err)
	}
	right, err := through(live)
	if err != nil {
		return "", fmt.Errorf("the cluster's: %w", err)
	}
	var keys []string
	for key, value := range left {
		if other, ok := right[key]; !ok || !bytes.Equal(value, other) {
			keys = append(keys, key)
		}
	}
	for key := range right {
		if _, ok := left[key]; !ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	sort.Strings(keys)
	return "spec." + keys[0], nil
}

// exact reports whether two values say the same, by the JSON both marshal
// to: keys in order, numbers as written.
func exact(said, live any, path string) (string, error) {
	left, err := canonicalValue(said)
	if err != nil {
		return "", err
	}
	right, err := canonicalValue(live)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(left, right) {
		return path, nil
	}
	return "", nil
}

func canonicalValue(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var read any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&read); err != nil {
		return nil, err
	}
	return json.Marshal(read)
}

// stated reports the first place live does not say what said states. said
// is the bundle's and live the cluster's; live may say more inside an
// object, and may not inside a list or inside a pipeline step's input.
func stated(said, live any, path string) (string, error) {
	switch want := said.(type) {
	case map[string]any:
		have, ok := live.(map[string]any)
		if !ok {
			if len(want) == 0 && live == nil {
				return "", nil
			}
			return path, nil
		}
		keys := make([]string, 0, len(want))
		for key := range want {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			at := path + "." + key
			if key == "input" && strings.HasPrefix(path, "spec.pipeline[") {
				if differs, err := exact(want[key], have[key], at); differs != "" || err != nil {
					return differs, err
				}
				continue
			}
			if differs, err := stated(want[key], have[key], at); differs != "" || err != nil {
				return differs, err
			}
		}
		return "", nil
	case []any:
		have, ok := live.([]any)
		if !ok {
			if len(want) == 0 && live == nil {
				return "", nil
			}
			return path, nil
		}
		if len(have) != len(want) {
			return path, nil
		}
		for i := range want {
			if differs, err := stated(want[i], have[i], path+"["+strconv.Itoa(i)+"]"); differs != "" || err != nil {
				return differs, err
			}
		}
		return "", nil
	case nil:
		// Stated as nothing: the API server keeps no null.
		return "", nil
	}
	return exact(said, live, path)
}
