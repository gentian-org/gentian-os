/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// schema-check names what of a deployments repository the platform's schema
// in this image no longer takes: every object of the gentianos.io group in
// the YAML files under each directory given, judged by
// schemacheck.(*Set).CheckRepository. One line per finding,
//
//	clusters/acme/tenants/team/groups.yaml: Tenant/team: .spec.groups: field not declared in schema
//
// and exit status 1 when there is any, 2 when a directory cannot be read.
// A deployments repository runs it in its CI against the image it is about
// to be installed with:
//
//	docker run --rm -v "$PWD:/repo:ro" ghcr.io/gentian-org/gentian-os:develop /schema-check /repo
package main

import (
	"fmt"
	"os"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: schema-check <directory>...")
		os.Exit(2)
	}
	set := schemacheck.Embedded()
	found := 0
	for _, dir := range os.Args[1:] {
		findings, err := set.CheckRepository(os.DirFS(dir))
		if err != nil {
			fmt.Fprintf(os.Stderr, "schema-check: %s: %v\n", dir, err)
			os.Exit(2)
		}
		for _, f := range findings {
			fmt.Println(f)
		}
		found += len(findings)
	}
	if found > 0 {
		fmt.Fprintf(os.Stderr, "schema-check: %d finding(s): this image's schema no longer takes them\n", found)
		os.Exit(1)
	}
}
