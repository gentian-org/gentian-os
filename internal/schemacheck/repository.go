/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Finding is one thing of a repository's file a set's schema no longer has.
type Finding struct {
	// File is the file's path in the repository.
	File string
	// Object is Kind/name, or Kind alone for an object that names none (a
	// kustomize patch always names one).
	Object string
	// What is the field or value, or that the kind is not served.
	What string
}

func (f Finding) String() string { return f.File + ": " + f.Object + ": " + f.What }

// group is the API group whose objects a repository is checked for.
const group = "gentianos.io"

// CheckRepository judges every object of the platform's API group in the
// YAML files under a repository's root by what the set takes away (Removed):
// a field the schema does not declare, a value its enums no longer list, a
// kind or a version the set does not serve. A kustomize patch is judged as
// what it is, a part of an object, so a field it leaves out is never
// reported. Objects of other groups, and files that are not YAML, are left
// alone; a file that does not parse is named.
//
// It is what a deployments repository asks of the image it is about to be
// installed with: an object the release no longer takes is refused by Argo
// CD's diff, and every later change to that object waits behind it.
func (s *Set) CheckRepository(root fs.FS) ([]Finding, error) {
	var out []Finding
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		found, err := s.checkFile(raw)
		if err != nil {
			out = append(out, Finding{File: p, Object: "-", What: "does not parse: " + err.Error()})
			return nil
		}
		for _, f := range found {
			f.File = p
			out = append(out, f)
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, err
}

func (s *Set) checkFile(raw []byte) ([]Finding, error) {
	var out []Finding
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		apiVersion, _ := obj["apiVersion"].(string)
		if !strings.HasPrefix(apiVersion, group+"/") {
			continue
		}
		kind, _ := obj["kind"].(string)
		name := kind
		if meta, ok := obj["metadata"].(map[string]any); ok {
			if n, ok := meta["name"].(string); ok && n != "" {
				name = kind + "/" + n
			}
		}
		def, ok := s.Kind(kind)
		if !ok {
			out = append(out, Finding{Object: name, What: fmt.Sprintf("the kind %s is not served", kind)})
			continue
		}
		if version := strings.TrimPrefix(apiVersion, group+"/"); def.Versions[version] == nil {
			out = append(out, Finding{Object: name, What: fmt.Sprintf("%s is not served at version %q", kind, version)})
			continue
		}
		removed, err := s.Removed(obj)
		if err != nil {
			return nil, err
		}
		for _, what := range removed {
			out = append(out, Finding{Object: name, What: what})
		}
	}
}
