/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	structuralpruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	fieldpath "k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// Refused says why an object of one of the set's kinds would not be taken by
// a cluster that serves this set's schema, or nothing where it would: a field
// the schema does not declare (which the API server prunes, and Argo CD's
// server-side diff refuses), a value its types and enums do not admit, and a
// rule (x-kubernetes-validations) it breaks. The object is the decoded YAML
// or JSON of one manifest. As the API server does, it fills in the schema's
// defaults before it validates: a rule may read a field the object leaves to
// its default.
func (s *Set) Refused(obj map[string]any) ([]string, error) {
	pruned, invalid, err := s.judge(obj)
	if err != nil {
		return nil, err
	}
	return append(pruned, invalid...), nil
}

// Removed says what of an object a cluster serving this set's schema no
// longer knows: a field the schema does not declare, or a value its enums do
// not list. It is Refused narrowed to what a release takes away; a rule the
// object breaks, or a value of the wrong type, is not reported.
func (s *Set) Removed(obj map[string]any) ([]string, error) {
	pruned, invalid, err := s.judge(obj)
	if err != nil {
		return nil, err
	}
	for _, e := range invalid {
		if strings.Contains(e, "Unsupported value") {
			pruned = append(pruned, e)
		}
	}
	return pruned, nil
}

// Invalid says what the API server would refuse of an object it is asked to
// store: what the object holds once the fields the schema does not declare
// are pruned and its defaults are filled in, measured against its types,
// enums and rules. A field the schema does not declare is not reported; the
// API server drops it rather than refusing the object.
func (s *Set) Invalid(obj map[string]any) ([]string, error) {
	_, invalid, err := s.judge(obj)
	return invalid, err
}

// admitter is what one kind at one version is judged with, compiled once.
type admitter struct {
	ss  *structuralschema.Structural
	sv  validation.SchemaValidator
	cel *cel.Validator
}

// admitters holds the compiled admitters of a set, by kind and version.
type admitters struct {
	mu sync.Mutex
	m  map[string]*admitter
}

func (s *Set) admitter(kind, version string) (*admitter, bool, error) {
	def, ok := s.Kind(kind)
	if !ok {
		return nil, false, fmt.Errorf("the set has no kind %q", kind)
	}
	v1, ok := def.Versions[version]
	if !ok {
		return nil, false, nil
	}
	key := kind + "/" + version
	if s.admit != nil {
		s.admit.mu.Lock()
		defer s.admit.mu.Unlock()
		if a, ok := s.admit.m[key]; ok {
			return a, true, nil
		}
	}
	var props apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v1, &props, nil); err != nil {
		return nil, false, err
	}
	ss, err := structuralschema.NewStructural(&props)
	if err != nil {
		return nil, false, err
	}
	sv, _, err := validation.NewSchemaValidator(&props)
	if err != nil {
		return nil, false, err
	}
	a := &admitter{ss: ss, sv: sv, cel: cel.NewValidator(ss, true, celconfig.PerCallLimit)}
	if s.admit != nil {
		if s.admit.m == nil {
			s.admit.m = map[string]*admitter{}
		}
		s.admit.m[key] = a
	}
	return a, true, nil
}

// judge returns the fields the schema does not declare, and what else the
// API server would refuse of the object once pruned and defaulted.
func (s *Set) judge(obj map[string]any) (pruned, invalid []string, err error) {
	kind, _ := obj["kind"].(string)
	apiVersion, _ := obj["apiVersion"].(string)
	version := apiVersion[strings.LastIndex(apiVersion, "/")+1:]
	a, served, err := s.admitter(kind, version)
	if err != nil {
		return nil, nil, err
	}
	if !served {
		return nil, []string{fmt.Sprintf("%s is not served at version %q", kind, version)}, nil
	}

	// What the schema does not declare. The copy keeps the object as it was.
	copied := deepCopy(obj).(map[string]any)
	pruned = structuralpruning.PruneWithOptions(copied, a.ss, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	sort.Strings(pruned)
	for i, path := range pruned {
		pruned[i] = path + ": field not declared in schema"
	}
	structuraldefaulting.Default(copied, a.ss)

	for _, e := range validation.ValidateCustomResource(fieldpath.NewPath(""), copied, a.sv) {
		invalid = append(invalid, e.Error())
	}
	errs, _ := a.cel.Validate(
		context.Background(), fieldpath.NewPath(""), a.ss, copied, nil, celconfig.RuntimeCELCostBudget)
	for _, e := range errs {
		invalid = append(invalid, e.Error())
	}
	return pruned, invalid, nil
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
