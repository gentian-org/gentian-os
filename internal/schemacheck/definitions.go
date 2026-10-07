/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package schemacheck compares the resource definitions a binary was built
// with against the ones a cluster serves.
//
// An API server validates and prunes an object against the definition it
// holds, not the one the writer had in mind. When the cluster's definition is
// older than the software -- Argo CD following a branch that moved back under
// a pinned image, a `helm upgrade` (which never touches crds/), a new image
// rolled without re-applying the Crossplane definitions -- every field the
// cluster does not know is dropped on write, and nothing says so: the write
// succeeds, and the object simply lacks what was written. A pruned addon pin
// is an add-on running unverified; a pruned pull secret is a private chart
// that is never pulled.
//
// This package holds the half both binaries share: the definitions embedded
// at build time, the comparison, the report of it, and the reading of a
// manifest against it. Reading the cluster and holding reconcilers is the
// operator's (the cluster subpackage); the director only ever asks the
// operator what it found.
package schemacheck

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Group is the API group every checked definition belongs to.
const Group = "gentianos.io"

// The definitions, as they are in the repository: charts/gentian-os/crds and
// crossplane/xrds, copied by `make manifests` (scripts/gen/gen-definitions.py)
// and held to the originals by verify-gen. Nothing here is a list of fields;
// the fields are whatever these files declare.
//
//go:embed definitions/crds/*.yaml definitions/xrds/*.yaml
var definitions embed.FS

// Where a definition reaches a cluster from, which is also what says how an
// outdated one is brought up to date.
const (
	// SourceChart is a CRD in charts/gentian-os/crds: applied by the Argo CD
	// sync that also rolls the operator and the director.
	SourceChart = "chart"
	// SourceXRD is a CRD Crossplane generates from an XRD in crossplane/xrds:
	// applied only by installer step B-06.
	SourceXRD = "xrd"
)

// Remedies, by source. They are what a person is told to do, so they are
// stated once and used by the log line, the Event, the condition and the
// director's refusal alike.
const (
	RemedyChart = "sync the gentian-os Application in Argo CD at the revision this software was built from"
	RemedyXRD   = "re-run installer step B-06 from a current checkout: ./install.sh --only B-06"
)

// Definition is one CustomResourceDefinition the cluster is expected to
// serve, with the schema the binary was built with.
type Definition struct {
	// Kind is the kind the CRD defines.
	Kind string
	// CRD is the name of the CustomResourceDefinition object.
	CRD string
	// Source is SourceChart or SourceXRD.
	Source string
	// XRD is the CompositeResourceDefinition a SourceXRD definition is
	// generated from. A claim and its composite share one.
	XRD string
	// Versions is the schema of every version the definition serves.
	Versions map[string]*apiextensionsv1.JSONSchemaProps
}

// Remedy is what brings an outdated copy of this definition up to date.
func (d Definition) Remedy() string {
	if d.Source == SourceXRD {
		return RemedyXRD
	}
	return RemedyChart
}

// Set is every definition a binary was built with.
type Set struct {
	// Definitions is sorted by CRD name.
	Definitions []Definition
	// Digest identifies the set: the SHA-256 of the embedded files. Two
	// binaries with the same digest check, and write, the same fields.
	Digest string

	byKind map[string]int
}

// Kind returns the definition of a kind.
func (s *Set) Kind(kind string) (Definition, bool) {
	i, ok := s.byKind[kind]
	if !ok {
		return Definition{}, false
	}
	return s.Definitions[i], true
}

// Kinds is every kind in the set, sorted.
func (s *Set) Kinds() []string {
	out := make([]string, 0, len(s.byKind))
	for k := range s.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CRDNames is the name of every CustomResourceDefinition in the set, sorted.
func (s *Set) CRDNames() []string {
	out := make([]string, 0, len(s.Definitions))
	for _, d := range s.Definitions {
		out = append(out, d.CRD)
	}
	return out
}

var (
	embeddedOnce sync.Once
	embeddedSet  *Set
	errEmbedded  error
)

// Embedded is the set this binary was built with.
//
// It panics when the embedded files do not parse. They are the repository's
// own definitions, checked by the tests of this package, so that is a broken
// build and not a condition to run under.
func Embedded() *Set {
	embeddedOnce.Do(func() { embeddedSet, errEmbedded = Load(definitions) })
	if errEmbedded != nil {
		panic("schemacheck: the embedded definitions do not parse: " + errEmbedded.Error())
	}
	return embeddedSet
}

// Load reads a set from a file system laid out as the embedded one is:
// definitions/crds holding CRDs, definitions/xrds holding XRDs.
func Load(fsys fs.FS) (*Set, error) {
	set := &Set{byKind: map[string]int{}}
	sum := sha256.New()
	for _, dir := range []string{"definitions/crds", "definitions/xrds"} {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			raw, err := fs.ReadFile(fsys, dir+"/"+e.Name())
			if err != nil {
				return nil, err
			}
			// Name and length before the bytes, so that moving bytes from
			// one file to the next cannot leave the digest unchanged.
			_, _ = fmt.Fprintf(sum, "%s %d\n", e.Name(), len(raw))
			_, _ = sum.Write(raw)
			defs, err := parse(raw)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", dir, e.Name(), err)
			}
			set.Definitions = append(set.Definitions, defs...)
		}
	}
	sort.Slice(set.Definitions, func(i, j int) bool { return set.Definitions[i].CRD < set.Definitions[j].CRD })
	for i, d := range set.Definitions {
		if _, dup := set.byKind[d.Kind]; dup {
			return nil, fmt.Errorf("kind %s is defined twice", d.Kind)
		}
		set.byKind[d.Kind] = i
	}
	if len(set.Definitions) == 0 {
		return nil, errors.New("no definitions")
	}
	set.Digest = "sha256:" + hex.EncodeToString(sum.Sum(nil))
	return set, nil
}

// parse reads every definition out of one file. A file may hold several
// documents -- the XRD files carry the ClusterRoles their Compositions need --
// and everything that is neither a CRD nor an XRD is passed over.
func parse(raw []byte) ([]Definition, error) {
	var out []Definition
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil {
			return nil, err
		}
		switch head.Kind {
		case "CustomResourceDefinition":
			def, err := fromCRD(doc)
			if err != nil {
				return nil, err
			}
			out = append(out, def)
		case "CompositeResourceDefinition":
			defs, err := fromXRD(doc)
			if err != nil {
				return nil, err
			}
			out = append(out, defs...)
		}
	}
}

func fromCRD(doc []byte) (Definition, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(doc, &crd); err != nil {
		return Definition{}, err
	}
	def := Definition{
		Kind: crd.Spec.Names.Kind, CRD: crd.Name, Source: SourceChart,
		Versions: map[string]*apiextensionsv1.JSONSchemaProps{},
	}
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if !v.Served || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		def.Versions[v.Name] = v.Schema.OpenAPIV3Schema
	}
	if def.Kind == "" || def.CRD == "" || len(def.Versions) == 0 {
		return Definition{}, fmt.Errorf("CRD %q names no kind or serves no version with a schema", crd.Name)
	}
	return def, nil
}

// xrd is as much of a CompositeResourceDefinition as is read here. Crossplane's
// own type is not imported for five fields.
type xrd struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind string `json:"kind"`
		} `json:"names"`
		ClaimNames *struct {
			Kind   string `json:"kind"`
			Plural string `json:"plural"`
		} `json:"claimNames"`
		Versions []struct {
			Name   string `json:"name"`
			Served bool   `json:"served"`
			Schema struct {
				OpenAPIV3Schema runtime.RawExtension `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

// crossplaneOwned is the fields Crossplane defines itself on every composite
// and claim it generates, whatever the XRD says under the same name.
var crossplaneOwned = map[string][]string{
	"spec": {
		"compositionRef", "compositionSelector", "compositionRevisionRef", "compositionRevisionSelector",
		"compositionUpdatePolicy", "compositeDeletePolicy", "resourceRef", "resourceRefs", "claimRef",
		"writeConnectionSecretToRef", "crossplane",
	},
	"status": {"conditions", "connectionDetails", "claimConditionTypes"},
}

// fromXRD returns the CRDs Crossplane generates from an XRD: the composite,
// and the claim when the XRD offers one.
//
// Those generated CRDs are what is checked, not the XRD object. The API
// server has never heard of an XRD: it validates and prunes an App, an
// XTenant or a Cluster against the CustomResourceDefinition Crossplane wrote
// for it. An XRD that is current while its CRD is not -- Crossplane not
// having caught up, or failing to -- prunes exactly like an old XRD, and only
// the CRD shows it.
//
// Of the XRD's schema Crossplane carries the properties of spec and of status
// into the generated CRD, and anything else at the top level it replaces with
// its own, so only those two subtrees are required of the cluster. It then
// writes its own machinery fields over any of the same name (crossplaneOwned),
// so those are not required either: an XRD that declared one would be
// overruled in the CRD, and this check would call a current cluster outdated
// for good. (Crossplane 2.2, internal/xcrd: genCrdVersion and schemas.go.)
func fromXRD(doc []byte) ([]Definition, error) {
	var x xrd
	if err := yaml.Unmarshal(doc, &x); err != nil {
		return nil, err
	}
	versions := map[string]*apiextensionsv1.JSONSchemaProps{}
	for _, v := range x.Spec.Versions {
		if !v.Served || len(v.Schema.OpenAPIV3Schema.Raw) == 0 {
			continue
		}
		var full apiextensionsv1.JSONSchemaProps
		if err := yaml.Unmarshal(v.Schema.OpenAPIV3Schema.Raw, &full); err != nil {
			return nil, fmt.Errorf("XRD %s version %s: %w", x.Metadata.Name, v.Name, err)
		}
		carried := &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{}}
		for _, part := range []string{"spec", "status"} {
			sub, ok := full.Properties[part]
			if !ok {
				continue
			}
			for _, name := range crossplaneOwned[part] {
				delete(sub.Properties, name)
			}
			carried.Properties[part] = sub
		}
		versions[v.Name] = carried
	}
	if x.Metadata.Name == "" || x.Spec.Names.Kind == "" || x.Spec.Group == "" || len(versions) == 0 {
		return nil, fmt.Errorf("XRD %q names no kind or group, or serves no version with a schema", x.Metadata.Name)
	}
	out := []Definition{{
		Kind: x.Spec.Names.Kind, CRD: x.Metadata.Name, Source: SourceXRD, XRD: x.Metadata.Name, Versions: versions,
	}}
	if c := x.Spec.ClaimNames; c != nil && c.Kind != "" && c.Plural != "" {
		out = append(out, Definition{
			Kind: c.Kind, CRD: c.Plural + "." + x.Spec.Group, Source: SourceXRD, XRD: x.Metadata.Name, Versions: versions,
		})
	}
	return out, nil
}
