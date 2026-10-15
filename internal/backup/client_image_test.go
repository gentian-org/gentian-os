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

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The Jobs that administer the object store run the image its server runs,
// and no other: the one image the cluster already pulls, with weed shell of
// the server's own version. The server's pin is the kernel's SeaweedFS
// values; this holds the Jobs' to it, so moving the server moves the Jobs in
// the same commit or fails here.
func TestTheObjectStorageJobsRunTheImageTheS3ServerRuns(t *testing.T) {
	manifest := filepath.Join("..", "..", "kernel", "services", "infra-seaweedfs", "manifests", "templates", "configmap.yaml")
	text, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read %s: %v", manifest, err)
	}
	m := regexp.MustCompile(`(?m)^      imageOverride: (\S+)$`).FindSubmatch(text)
	if m == nil {
		t.Fatalf("%s: no master.imageOverride in the shape this test reads", manifest)
	}
	server := string(m[1])
	if !regexp.MustCompile(`:[^:@/]+@sha256:[0-9a-f]{64}$`).MatchString(server) {
		t.Errorf("the server's image %s is not named by tag and digest", server)
	}
	if kernel.DefaultSeaweedFSImage != server {
		t.Errorf("kernel.DefaultSeaweedFSImage = %q, but the S3 server runs %q (%s): the Jobs must run the server's image",
			kernel.DefaultSeaweedFSImage, server, manifest)
	}
	demo := tenant("demo", nil)
	for name, image := range map[string]string{
		"the bucket Job":  ObjectStorageProvisionContainer("create-bucket", "demo-wiki", "AK", "SK").Image,
		"the destroy Job": ObjectStorageDestroyJob(demo, "wiki", DestroyWithinARequest).Spec.Template.Spec.Containers[0].Image,
	} {
		if image != server {
			t.Errorf("%s runs %s, not the server's %s", name, image, server)
		}
	}
}
