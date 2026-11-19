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

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
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
// or JSON of one manifest.
func (s *Set) Refused(obj map[string]any) ([]string, error) {
	kind, _ := obj["kind"].(string)
	apiVersion, _ := obj["apiVersion"].(string)
	def, ok := s.Kind(kind)
	if !ok {
		return nil, fmt.Errorf("the set has no kind %q", kind)
	}
	version := apiVersion[strings.LastIndex(apiVersion, "/")+1:]
	v1, ok := def.Versions[version]
	if !ok {
		return []string{fmt.Sprintf("%s is not served at version %q", kind, version)}, nil
	}
	var props apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v1, &props, nil); err != nil {
		return nil, err
	}
	ss, err := structuralschema.NewStructural(&props)
	if err != nil {
		return nil, err
	}
	var out []string

	// What the schema does not declare. The copy keeps the object as it was.
	copied := deepCopy(obj).(map[string]any)
	pruned := structuralpruning.PruneWithOptions(copied, ss, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	sort.Strings(pruned)
	for _, path := range pruned {
		out = append(out, path+": field not declared in schema")
	}

	sv, _, err := validation.NewSchemaValidator(&props)
	if err != nil {
		return nil, err
	}
	for _, e := range validation.ValidateCustomResource(fieldpath.NewPath(""), copied, sv) {
		out = append(out, e.Error())
	}
	errs, _ := cel.NewValidator(ss, true, celconfig.PerCallLimit).Validate(
		context.Background(), fieldpath.NewPath(""), ss, copied, nil, celconfig.RuntimeCELCostBudget)
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out, nil
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
