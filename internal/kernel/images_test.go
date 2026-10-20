/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package kernel

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Jobs that move a tenant's objects run the storage tools CI built in the
// same run as the operator that made them: the operator's chart names the
// tools at the operator's own image tag. A tag of their own would let the
// scripts and the tools they call come from different commits.
func TestTheStorageToolsFollowTheOperatorsTag(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is needed to render the operator chart: %v", err)
	}
	chart := filepath.Join("..", "..", "charts", "gentian-os")
	meta, err := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	appVersion := regexp.MustCompile(`(?m)^appVersion: "?([^"\n]+)"?$`).FindSubmatch(meta)
	if appVersion == nil {
		t.Fatal("the operator chart has no appVersion")
	}
	for _, c := range []struct {
		set  []string
		want string
	}{
		{nil, StorageToolsRepository + ":" + string(appVersion[1])},
		{[]string{"image.tag=develop-0123abc"}, StorageToolsRepository + ":develop-0123abc"},
		{[]string{"image.tag=develop-0123abc", "image.digest=sha256:" + strings.Repeat("0", 64)}, StorageToolsRepository + ":develop-0123abc"},
		{[]string{"provisioners.storageTools=registry.example.org/tools:1"}, "registry.example.org/tools:1"},
	} {
		args := []string{"template", "gentian-os", chart, "-s", "templates/deployment.yaml"}
		for _, s := range c.set {
			args = append(args, "--set", s)
		}
		var stderr bytes.Buffer
		cmd := exec.Command("helm", args...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("helm template %v: %v\n%s", c.set, err, stderr.String())
		}
		got := regexp.MustCompile(`- name: STORAGE_TOOLS_IMAGE\n\s+value: "([^"]*)"`).FindSubmatch(out)
		if got == nil {
			t.Fatalf("%v: the operator is not told STORAGE_TOOLS_IMAGE", c.set)
		}
		if string(got[1]) != c.want {
			t.Errorf("%v: STORAGE_TOOLS_IMAGE = %s, want %s", c.set, got[1], c.want)
		}
	}
}
