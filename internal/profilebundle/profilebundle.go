/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package profilebundle is how the operator knows that the ComponentProfile
// it is about to roll out is the build an install was pinned to.
//
// An install from a catalogue source is pinned to a digest: the sha256 of the
// entry's file exactly as the source published it -- the profile and, after
// it, whatever travels with it (bundle.go). The director checks the
// bytes it fetched against that digest before it commits anything. What then
// reaches the cluster is not those bytes: Argo CD applies the document, the
// API server prunes and defaults it, and the object that comes back has no
// byte sequence anybody hashed. So the digest on a Component cannot be
// recomputed from the profile in the cluster, and comparing it with a second
// copy of itself would prove nothing.
//
// What is kept instead is the bundle: the director commits the verified bytes
// beside the profile, and they arrive as an annotation on it. The operator
// trusts neither the annotation nor whoever wrote it. It hashes the bytes
// itself, which only the build the install named can satisfy, and then checks
// that the profile in the cluster is what those bytes say -- read the way the
// operator reads a profile, with the API server's defaults applied. Both have
// to hold. Bytes that hash correctly beside a profile that says something
// else are a mismatch, and so is a profile nobody can show the bytes for.
//
// The same goes for every companion the bundle brings: it is looked up in the
// cluster and compared with what the bytes say (companions.go), and one that
// is missing or different holds the rollout as the profile would.
package profilebundle

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Annotation carries the bundle on a ComponentProfile: the bytes the source
// served and the director verified, base64-encoded so that not one of them
// changes on the way through git, kustomize and the API server.
const Annotation = "gentianos.io/profile-bundle"

// ownedPrefix is the namespace of the labels and annotations the platform
// acts on. A profile may carry others -- Argo CD's tracking is one -- and
// they say nothing about what is rolled out.
const ownedPrefix = "gentianos.io/"

// MaxBytes bounds a bundle that can be carried: the whole file, the profile
// and its companions. An object's annotations may total 256 KiB and base64
// makes four bytes of three, so this leaves room for the profile's own
// annotations. The largest bundle published, a profile with its Composition,
// is under half of it.
const MaxBytes = 180 << 10

// Reasons a profile is not the build its Component is pinned to. They are the
// condition reasons a Component reports.
const (
	// ReasonMismatch: the bundle hashes to another digest, or the profile in
	// the cluster is not what the bundle says.
	ReasonMismatch = "DigestMismatch"
	// ReasonUnverifiable: there is nothing to check the profile against.
	ReasonUnverifiable = "DigestUnverifiable"
	// ReasonRefused: the bundle is the pinned build and holds what a bundle
	// may not (bundle.go).
	ReasonRefused = "BundleRefused"
	// ReasonCompanionMissing: an object the bundle brings beside its profile
	// is not in the cluster.
	ReasonCompanionMissing = "CompanionMissing"
	// ReasonCompanionMismatch: one is there and is not what the bundle says.
	ReasonCompanionMismatch = "CompanionMismatch"
)

// Refusal is a profile that is not shown to be the build a digest names.
type Refusal struct {
	// Reason is ReasonMismatch or ReasonUnverifiable.
	Reason string
	// Message says what was expected and what was found, for a person.
	Message string
	// Retry says the refusal may end without the profile or the pin
	// changing, so whoever was refused looks again: a companion Argo CD has
	// not applied yet arrives on its own.
	Retry bool
}

// Encode is the annotation's value for a bundle.
func Encode(bundle []byte) string {
	return base64.StdEncoding.EncodeToString(bundle)
}

// Digest is the digest of a bundle, by the definition the catalogue's index
// uses: "sha256:" and the hex of the sha256 of the published file's bytes.
func Digest(bundle []byte) string {
	sum := sha256.Sum256(bundle)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Short is the first twelve hex characters of a digest, which tell two builds
// of an entry apart without filling a line.
func Short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "sha256:" + d
}

// Verify reports whether profile is the build digest names. Nil means it is:
// the bundle the profile carries hashes to digest, and the profile is what
// that bundle says. Anything else is a Refusal, and there is no third
// answer: what cannot be checked is refused.
//
// digest is "sha256:<hex>", as a Component's profileRef carries it.
func Verify(profile *gentianov1alpha1.ComponentProfile, digest string) *Refusal {
	want := strings.ToLower(strings.TrimSpace(digest))
	encoded, ok := profile.Annotations[Annotation]
	if !ok || strings.TrimSpace(encoded) == "" {
		return &Refusal{Reason: ReasonUnverifiable, Message: fmt.Sprintf(
			"the install is pinned to %s, and ComponentProfile %q carries no bundle to check that against (annotation %s)",
			Short(want), profile.Name, Annotation)}
	}
	bundle, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return &Refusal{Reason: ReasonUnverifiable, Message: fmt.Sprintf(
			"the install is pinned to %s, and the bundle ComponentProfile %q carries cannot be read: %v",
			Short(want), profile.Name, err)}
	}
	if found := Digest(bundle); found != want {
		return &Refusal{Reason: ReasonMismatch, Message: fmt.Sprintf(
			"the install is pinned to %s, and the bundle of ComponentProfile %q is %s",
			Short(want), profile.Name, Short(found))}
	}

	// The bytes are the build. Whether they are a bundle at all is asked
	// again here, as the director asked it before it committed them: what is
	// in the cluster is not taken to have passed through the director.
	read, err := Check(bundle, profile.Name, profile.Annotations[OriginAnnotation])
	if err != nil {
		reason := ReasonRefused
		if docs, parseErr := documents(bundle); parseErr != nil || len(docs) == 0 {
			reason = ReasonUnverifiable
		} else if head := headOf(docs[0]); head.kind != "ComponentProfile" || head.name != profile.Name {
			return &Refusal{Reason: ReasonMismatch, Message: fmt.Sprintf(
				"the bundle of ComponentProfile %q is %s as pinned, and is the %s %q",
				profile.Name, Short(want), head.kind, head.name)}
		}
		return &Refusal{Reason: reason, Message: fmt.Sprintf(
			"the bundle of ComponentProfile %q is %s as pinned, and is not one that is rolled out: %v",
			profile.Name, Short(want), err)}
	}
	// Whether the profile is the build, is the other half.
	said, err := decode(read.Profile)
	if err != nil {
		return &Refusal{Reason: ReasonUnverifiable, Message: fmt.Sprintf(
			"the bundle of ComponentProfile %q is %s as pinned, and cannot be read as a profile: %v",
			profile.Name, Short(want), err)}
	}
	differs, err := specDiffers(&said.Spec, &profile.Spec)
	if err != nil {
		return &Refusal{Reason: ReasonUnverifiable, Message: fmt.Sprintf(
			"ComponentProfile %q could not be compared with its bundle: %v", profile.Name, err)}
	}
	if len(differs) > 0 {
		return &Refusal{Reason: ReasonMismatch, Message: fmt.Sprintf(
			"the bundle of ComponentProfile %q is %s as pinned, and the profile in the cluster is not what it says: spec.%s differs",
			profile.Name, Short(want), strings.Join(differs, ", spec."))}
	}
	if key := metadataDiffers(said, profile); key != "" {
		return &Refusal{Reason: ReasonMismatch, Message: fmt.Sprintf(
			"the bundle of ComponentProfile %q is %s as pinned, and the profile in the cluster is not what it says: %s differs",
			profile.Name, Short(want), key)}
	}
	return nil
}

// decode reads a bundle's profile as the operator would read it from the
// cluster: the document (as documents read it), with the defaults the API
// server applies, into the type every reconciler reads a profile through. A
// field the type does not have is dropped here exactly as it is dropped there.
func decode(read map[string]any) (*gentianov1alpha1.ComponentProfile, error) {
	// A copy: the defaults are written into it, and the bundle is read again.
	raw, err := json.Marshal(read)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := utiljson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	schema, err := profileSchema()
	if err != nil {
		return nil, err
	}
	applyDefaults(doc, schema)
	defaulted, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	out := &gentianov1alpha1.ComponentProfile{}
	if err := json.Unmarshal(defaulted, out); err != nil {
		return nil, err
	}
	return out, nil
}

// specDiffers names the top-level fields of spec in which two profiles
// differ, compared as the JSON both marshal to. Through the same type and the
// same encoder, so an empty list and an absent one are the same thing on both
// sides, and a free-form value is compared by what it says rather than by how
// its keys happened to be ordered.
func specDiffers(a, b *gentianov1alpha1.ComponentProfileSpec) ([]string, error) {
	left, err := canonical(a)
	if err != nil {
		return nil, err
	}
	right, err := canonical(b)
	if err != nil {
		return nil, err
	}
	var out []string
	for key, value := range left {
		if other, ok := right[key]; !ok || !bytes.Equal(value, other) {
			out = append(out, key)
		}
	}
	for key := range right {
		if _, ok := left[key]; !ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// canonical is a value's JSON with every object's keys in order and every
// number as it was written, one entry per top-level field.
func canonical(v any) (map[string][]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Not float64: two integers past 2^53 would otherwise compare equal.
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(fields))
	for key, value := range fields {
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out[key] = b
	}
	return out, nil
}

// metadataDiffers names a label or annotation the platform acts on that the
// profile in the cluster does not have as the bundle states it, or has
// without the bundle stating it. Empty means none.
//
// Everything the bundle states has to be there unchanged. Beyond that, only
// keys outside the platform's own namespace may be added -- Argo CD's
// tracking, a kustomization's labels -- because nothing here reads them.
func metadataDiffers(said, live *gentianov1alpha1.ComponentProfile) string {
	for _, pair := range []struct {
		kind       string
		said, live map[string]string
	}{
		{"label", said.Labels, live.Labels},
		{"annotation", said.Annotations, live.Annotations},
	} {
		var keys []string
		for key, value := range pair.said {
			if key == Annotation {
				continue
			}
			if got, ok := pair.live[key]; !ok || got != value {
				keys = append(keys, key)
			}
		}
		for key := range pair.live {
			if key == Annotation || !strings.HasPrefix(key, ownedPrefix) {
				continue
			}
			// Where the profile came from is written beside the bundle by
			// the director, like the bundle itself, and is not something a
			// source states. A bundle that does state it is compared above,
			// like anything else it says.
			if key == OriginAnnotation && pair.kind == "annotation" {
				continue
			}
			if _, ok := pair.said[key]; !ok {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			return pair.kind + " " + keys[0]
		}
	}
	return ""
}

// The ComponentProfile CRD as this operator was built with it, which is where
// the defaults the API server applies are written down. A copy of
// config/crd/gentianos.io_componentprofiles.yaml, made by `make manifests`.
//
//go:embed componentprofiles.crd.yaml
var crd []byte

var (
	schemaOnce sync.Once
	schema     *apiextensionsv1.JSONSchemaProps
	schemaErr  error
)

// profileSchema is the schema of the version this operator reads.
func profileSchema() (*apiextensionsv1.JSONSchemaProps, error) {
	schemaOnce.Do(func() {
		var def apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(crd, &def); err != nil {
			schemaErr = fmt.Errorf("the ComponentProfile schema cannot be read: %w", err)
			return
		}
		for i := range def.Spec.Versions {
			v := &def.Spec.Versions[i]
			if v.Name == gentianov1alpha1.GroupVersion.Version && v.Schema != nil && v.Schema.OpenAPIV3Schema != nil {
				schema = v.Schema.OpenAPIV3Schema
				return
			}
		}
		schemaErr = fmt.Errorf("the ComponentProfile schema has no version %s", gentianov1alpha1.GroupVersion.Version)
	})
	return schema, schemaErr
}

// applyDefaults fills in the defaults a schema states, the way the API server
// does when it stores or serves an object: a property with a default is set
// where it is absent, or null where null is not allowed, and only inside
// objects that exist. Defaults that are themselves objects are then walked,
// so a default of {} brings its own properties' defaults with it.
func applyDefaults(value any, s *apiextensionsv1.JSONSchemaProps) {
	if s == nil {
		return
	}
	switch x := value.(type) {
	case map[string]any:
		for name := range s.Properties {
			prop := s.Properties[name]
			if prop.Default == nil {
				continue
			}
			if current, found := x[name]; found && (current != nil || prop.Nullable) {
				continue
			}
			var def any
			if err := utiljson.Unmarshal(prop.Default.Raw, &def); err == nil {
				x[name] = def
			}
		}
		for name, child := range x {
			if prop, found := s.Properties[name]; found {
				applyDefaults(child, &prop)
			} else if s.AdditionalProperties != nil {
				applyDefaults(child, s.AdditionalProperties.Schema)
			}
		}
	case []any:
		if s.Items == nil {
			return
		}
		for _, item := range x {
			applyDefaults(item, s.Items.Schema)
		}
	}
}
