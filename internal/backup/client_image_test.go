/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The Jobs that talk to object storage run the image the cluster's S3
// server runs, and no other: the one image the cluster already pulls, with
// the client on its PATH. The server's pin is the kernel's MinIO manifest;
// this holds MinIOClientImage to it, so moving the server moves the Jobs in
// the same commit or fails here.
func TestTheObjectStorageJobsRunTheImageTheS3ServerRuns(t *testing.T) {
	manifest := filepath.Join("..", "..", "kernel", "services", "infra-minio", "manifests", "templates", "configmap.yaml")
	text, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read %s: %v", manifest, err)
	}
	// The server's own image block, not volumePermissions' below it: the
	// first image: at the chart's top level.
	re := regexp.MustCompile(`(?s)\n    image:\n      registry: (\S+)\n      repository: (\S+)\n      tag: (\S+)\n`)
	m := re.FindSubmatch(text)
	if m == nil {
		t.Fatalf("%s: no image block in the shape this test reads", manifest)
	}
	server := string(m[1]) + "/" + string(m[2]) + ":" + string(m[3])
	if MinIOClientImage != server {
		t.Errorf("MinIOClientImage = %q, but the S3 server runs %q (%s): the Jobs must run the server's image", MinIOClientImage, server, manifest)
	}
}
