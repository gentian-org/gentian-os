/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"fmt"
	"strings"

	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What a bundle owns, for whoever asks what on a cluster no bundle owns.
//
// Nothing a bundle brought is taken away when a newer build stops bringing
// it, so a cluster can hold objects of the companion kinds that no bundle
// owns any more. Finding them needs the same three things the rollout check
// uses, said once here: which bundle a profile carries, which kinds a
// companion may be, and which name of each kind belongs to which profile.

// KindProfile is the kind of a bundle's first document.
const KindProfile = "ComponentProfile"

// Ref names one document of a bundle.
type Ref struct {
	APIVersion string
	Kind       string
	Name       string
}

// CompanionKind is one kind a bundle may hold beside its profile.
type CompanionKind struct {
	Kind string
	GVK  runtimeschema.GroupVersionKind
	// Namespaced says objects of the kind live in a namespace: the one the
	// catalogue is applied in.
	Namespaced bool
}

// CompanionKinds are the kinds a bundle may hold beside its profile, in a
// fixed order. It is the list of rules, read out.
func CompanionKinds() []CompanionKind {
	out := make([]CompanionKind, 0, len(rules))
	for _, kind := range []string{KindComposition, KindOIDCPacks, KindConfigMap, KindCustomization} {
		for key, r := range rules {
			if r.kind != kind {
				continue
			}
			apiVersion := strings.TrimSuffix(key, "/"+kind)
			out = append(out, CompanionKind{
				Kind: kind, GVK: runtimeschema.FromAPIVersionAndKind(apiVersion, kind), Namespaced: r.namespaced,
			})
		}
	}
	return out
}

// Carried reads the bundle a profile carries: the bytes the rollout check
// hashes, held to what a bundle may be. A profile that carries none answers
// nil and no error. One whose bundle cannot be read, or holds what a bundle
// may not, answers the error: what it owns is then not known.
func Carried(profile *gentianov1alpha1.ComponentProfile) (*Bundle, error) {
	if strings.TrimSpace(profile.Annotations[Annotation]) == "" {
		return nil, nil
	}
	bundle, ok := carried(profile)
	if !ok {
		return nil, fmt.Errorf("the bundle ComponentProfile %q carries cannot be decoded", profile.Name)
	}
	return Check(bundle, profile.Name, profile.Annotations[OriginAnnotation])
}

// Declared names every document a bundle file holds, the profile included,
// without holding the file to any rule: for asking whether a file declares
// an object, whatever else is true of it.
func Declared(body []byte) ([]Ref, error) {
	docs, err := documents(body)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(docs))
	for _, doc := range docs {
		head := headOf(doc)
		out = append(out, Ref{APIVersion: head.apiVersion, Kind: head.kind, Name: head.name})
	}
	return out, nil
}

// PlatformComposition reports whether name is the Composition that renders
// every app that brings none: the platform's, and never a bundle's.
func PlatformComposition(name string) bool { return name == defaultComposition }

// ComposesApp reports whether a Composition composes the platform's app
// composite and nothing else: the only Compositions a bundle may bring.
func ComposesApp(composition map[string]any) bool {
	spec, _ := composition["spec"].(map[string]any)
	ref, _ := spec["compositeTypeRef"].(map[string]any)
	return ref["apiVersion"] == gentianAPIVersion && ref["kind"] == appComposite
}

// ShapeOwner is the profile whose bundle could bring a companion of this
// kind under this name, by the name alone: "app-<profile>" for a
// Composition, "<profile>-oidc" for an OIDCPackCatalog, "<profile>.<rest>"
// for a ConfigMap or a Customization. ok is false for a name no bundle could
// bring -- the platform's own Composition among them.
func ShapeOwner(kind, name string) (profile string, ok bool) {
	switch kind {
	case KindComposition:
		if PlatformComposition(name) {
			return "", false
		}
		profile, ok = strings.CutPrefix(name, compositionPrefix)
	case KindOIDCPacks:
		profile, ok = strings.CutSuffix(name, oidcSuffix)
	case KindConfigMap, KindCustomization:
		var rest string
		profile, rest, ok = strings.Cut(name, ".")
		ok = ok && dnsLabel.MatchString(rest) && name != rootCABundle
	}
	if !ok || !dnsLabel.MatchString(profile) {
		return "", false
	}
	return profile, true
}
