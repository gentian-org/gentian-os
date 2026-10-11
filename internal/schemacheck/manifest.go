/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// The director's half. It writes manifests to git, and what it writes is
// pruned when Argo CD applies it to a cluster whose definition is older. So
// which fields a write needs is read off the write itself: the manifest as
// it would be committed, against the manifest as it is. Nothing lists which
// route sets which field, and so nothing can fall behind the routes.

// Change is what one write would set in the manifests of one kind.
type Change struct {
	Kind string
	// Fields is the field paths the write sets or changes, sorted: present
	// in the manifest afterwards with a value it did not have before. A
	// field only removed is not in it -- taking a value away needs nothing
	// of the cluster.
	Fields []string
}

// Changes compares a manifest file before and after a write and returns what
// the write sets, per kind of this API group. before is nil for a new file.
//
// A document is read against the schema this binary was built with, which is
// what gives each value its field path. Kinds of other groups, and fields the
// built schema does not declare, are passed over: neither is this check's to
// vouch for. Kustomize patches are documents like any other here -- the ones
// the director writes name their target's apiVersion and kind and carry the
// fields they set.
func (s *Set) Changes(before, after []byte) ([]Change, error) {
	had, err := s.fieldsByDocument(before)
	if err != nil {
		return nil, fmt.Errorf("the manifest as it is: %w", err)
	}
	has, err := s.fieldsByDocument(after)
	if err != nil {
		return nil, fmt.Errorf("the manifest as it would be: %w", err)
	}
	perKind := map[string]map[string]bool{}
	for doc, fields := range has {
		// Counted, not just collected: two entries of a list holding the same
		// value are two values, and writing the second one is a change.
		budget := map[field]int{}
		for _, f := range had[doc] {
			budget[f]++
		}
		for _, f := range fields {
			if budget[f] > 0 {
				budget[f]--
				continue
			}
			if perKind[doc.kind] == nil {
				perKind[doc.kind] = map[string]bool{}
			}
			perKind[doc.kind][f.path] = true
		}
	}
	out := make([]Change, 0, len(perKind))
	for kind, paths := range perKind {
		c := Change{Kind: kind}
		for p := range paths {
			c.Fields = append(c.Fields, p)
		}
		sort.Strings(c.Fields)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}

type document struct{ kind, namespace, name string }

// field is one value at one field path. The path carries no list index, so
// that removing the first entry of a list does not read as a change to every
// entry after it; a map key is part of the value instead.
type field struct{ path, value string }

func (s *Set) fieldsByDocument(raw []byte) (map[document][]field, error) {
	out := map[document][]field{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		chunk, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var doc map[string]interface{}
		if err := yaml.Unmarshal(chunk, &doc); err != nil {
			return nil, err
		}
		apiVersion, _ := doc["apiVersion"].(string)
		group, version, ok := strings.Cut(apiVersion, "/")
		kind, _ := doc["kind"].(string)
		if !ok || group != Group || kind == "" {
			continue
		}
		def, ok := s.Kind(kind)
		if !ok {
			continue
		}
		schema, ok := def.Versions[version]
		if !ok {
			continue
		}
		key := document{kind: kind}
		if md, ok := doc["metadata"].(map[string]interface{}); ok {
			key.name, _ = md["name"].(string)
			key.namespace, _ = md["namespace"].(string)
		}
		for name, value := range doc {
			switch name {
			case "apiVersion", "kind", "metadata":
				// Never pruned: the API server keeps an object's metadata
				// whatever the definition says.
				continue
			}
			if sub, ok := schema.Properties[name]; ok {
				collect(name, "", value, &sub, out, key)
			}
		}
	}
}

func encode(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// collect walks a value with its schema and records every leaf it sets.
// keys is the map keys passed on the way, which are part of what the value
// is.
func collect(path, keys string, value interface{}, schema *apiextensionsv1.JSONSchemaProps, out map[document][]field, doc document) {
	leaf := func(p string, v interface{}) {
		out[doc] = append(out[doc], field{path: p, value: keys + encode(v)})
	}
	switch v := value.(type) {
	case nil:
		// A key with nothing under it sets nothing.
	case map[string]interface{}:
		for name, child := range v {
			switch sub, known := schema.Properties[name]; {
			case known:
				collect(join(path, name), keys, child, &sub, out, doc)
			case schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil:
				collect(path+"{}", keys+name+"=", child, schema.AdditionalProperties.Schema, out, doc)
			case preserves(schema) || (schema.AdditionalProperties != nil && schema.AdditionalProperties.Allows):
				// Free-form: the schema keeps whatever is put here.
				out[doc] = append(out[doc], field{path: join(path, "*"), value: keys + name + "=" + encode(child)})
			}
		}
	case []interface{}:
		// An empty list sets nothing either: it is what removing the last
		// entry leaves behind, and there is nothing in it to lose.
		if len(v) > 0 && (schema.Items == nil || schema.Items.Schema == nil) {
			leaf(path, v)
			return
		}
		for _, child := range v {
			collect(path+"[]", keys, child, schema.Items.Schema, out, doc)
		}
	default:
		leaf(path, v)
	}
}

// Refusal is a write that is not made, because the cluster would drop part
// of it or because that could not be ruled out.
type Refusal struct {
	// Unconfirmed is true when the state of the cluster's definitions could
	// not be learned, and false when it is known and the write needs a field
	// the cluster would drop.
	Unconfirmed bool
	// Kind and Fields name what the write needs, when that is known.
	Kind   string
	Fields []string
	// Message is the whole of it, for a person: the definition, the fields
	// and the remedy.
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// Source answers what the cluster serves. The director's asks the operator.
type Source func() (Report, error)

// Decide says whether a write that makes these changes may go ahead. Nil
// means it may.
//
// A write that sets nothing in a manifest of this API group -- a removal, or
// a file of another kind -- always may: nothing of it can be pruned.
//
// For the rest the answer follows the report, kind by kind: refused when a
// field the write sets is one the cluster would drop, and refused when what
// the cluster serves of that kind is not known. The second is the choice to
// fail closed: a write is not made on the assumption that the cluster is
// current. It covers an operator that cannot be reached, one that has not
// checked yet, and one built with other definitions than this binary -- its
// report is about its fields, not these.
//
// A kind the cluster does not have at all is not refused. Applying such a
// manifest fails where it can be seen; nothing is silently lost.
func Decide(set *Set, source Source, changes []Change) *Refusal {
	var touched []Change
	for _, c := range changes {
		if len(c.Fields) > 0 {
			touched = append(touched, c)
		}
	}
	if len(touched) == 0 {
		return nil
	}
	unconfirmed := func(why string) *Refusal {
		return &Refusal{
			Unconfirmed: true, Kind: touched[0].Kind, Fields: touched[0].Fields,
			Message: "it could not be confirmed that the cluster's resource definitions are as new as this software, " +
				"so nothing that sets a field is written: " + why,
		}
	}
	if source == nil {
		return unconfirmed("this director has no operator to ask")
	}
	report, err := source()
	switch {
	case err != nil:
		return unconfirmed("the operator could not be asked (" + err.Error() + "). Check that the operator is running; the change can be retried")
	case !report.Checked:
		return unconfirmed("the operator has not compared them yet. Retry shortly")
	case report.Definitions != set.Digest:
		return unconfirmed("the operator was built with other definitions than this director, which a rollout in progress explains. " +
			"Retry once both run the same release")
	}
	for _, c := range touched {
		kind, ok := report.Kind(c.Kind)
		if !ok {
			return unconfirmed("the operator reported nothing about " + c.Kind)
		}
		switch kind.State {
		case StateOK, StateAbsent:
		case StateOutdated:
			var dropped []string
			for _, f := range c.Fields {
				for _, m := range kind.Missing {
					if under(f, m) {
						dropped = append(dropped, f)
						break
					}
				}
			}
			if len(kind.Missing) == 0 {
				// Outdated without a field to name: the version is not served.
				dropped = c.Fields
			}
			if len(dropped) > 0 {
				return &Refusal{
					Kind: c.Kind, Fields: dropped,
					Message: fmt.Sprintf("the cluster's definition of %s (%s) is older than this software and would silently drop %s, "+
						"which this change sets; nothing was written. To fix: %s",
						kind.Kind, kind.CRD, fieldList(dropped), kind.Remedy),
				}
			}
		default:
			return unconfirmed(kind.Sentence())
		}
	}
	return nil
}
