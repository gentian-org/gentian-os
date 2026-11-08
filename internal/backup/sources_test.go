/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

// A fetch reads the source with the keys declared for it and writes with the
// platform's: two remotes, two Secrets, and no key on a command line.
func TestAFetchReadsTheSourceWithItsOwnKeys(t *testing.T) {
	job, err := BundleFetchJob(FetchParams{
		Namespace: "system-s3", Name: "bundle-fetch-1",
		From: gentianov1alpha1.BundleRef{Endpoint: "https://s3.example", Bucket: "b", Prefix: "corp/x",
			Region: "r1", AddressingStyle: gentianov1alpha1.S3AddressingVirtualHost, CredentialSecret: SourceCredentialName("old")},
		Bucket: "gentian-imports", Prefix: "20261015-120000-abcdef01",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	env := envOf(c)
	if env["SOURCE_ENDPOINT"].Value != "https://s3.example" || env["SOURCE_REGION"].Value != "r1" ||
		env["SOURCE_ADDRESSING_STYLE"].Value != "virtualHost" || env["SOURCE_PREFIX"].Value != "corp/x" {
		t.Errorf("source environment %+v", env)
	}
	if ref := env["SOURCE_SECRET_KEY"].ValueFrom.SecretKeyRef; ref.Name != "bundle-source-old" || ref.Key != DestinationSecretKeyField {
		t.Errorf("the source's key is read from %+v", ref)
	}
	if ref := env["S3_SECRET_KEY"].ValueFrom.SecretKeyRef; ref.Name != StorageAdminSecret {
		t.Errorf("the import storage's key is read from %+v", ref)
	}
	script := c.Args[0]
	for _, want := range []string{`RCLONE_CONFIG_SOURCE_ENDPOINT="${SOURCE_ENDPOINT}"`, `RCLONE_CONFIG_PLATFORM_ENDPOINT="${S3_ENDPOINT}"`,
		`rclone copy --checksum "source:${SOURCE_BUCKET}/${SOURCE_PREFIX}/"`} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	if job.Spec.Template.Labels[FetchLabel] != "bundle-fetch-1" || job.Spec.Template.Labels[meta.ComponentLabel] != FetchComponent {
		t.Errorf("the pod is not labeled for its way out: %v", job.Spec.Template.Labels)
	}

	if _, err := BundleFetchJob(FetchParams{Namespace: "system-s3", Name: "f",
		From:   gentianov1alpha1.BundleRef{Endpoint: "https://s3.example", Bucket: "b", Prefix: "x"},
		Bucket: "gentian-imports", Prefix: "p"}); err == nil {
		t.Error("a fetch from outside with no keys of its own was built")
	}
}

// A fetch of a bucket of the cluster's own storage reads it with the
// platform's keys, as everything inside does.
func TestAFetchFromInsideReadsWithThePlatformsKeys(t *testing.T) {
	job, err := BundleFetchJob(FetchParams{Namespace: "system-s3", Name: "f",
		From:   gentianov1alpha1.BundleRef{Bucket: "corp-gentian-backup", Prefix: "nightly-1"},
		Bucket: "gentian-imports", Prefix: "p"})
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(job.Spec.Template.Spec.Containers[0])
	if ref := env["SOURCE_ENDPOINT"].ValueFrom.SecretKeyRef; ref == nil || ref.Name != StorageAdminSecret {
		t.Errorf("the source's address is %+v", env["SOURCE_ENDPOINT"])
	}
}

func TestASourcesPrefixJoinsWithoutStraySlashes(t *testing.T) {
	for _, c := range []struct{ base, name, want string }{
		{"", "x", "x"}, {"corp/", "x", "corp/x"}, {"/corp", "/x/", "corp/x"},
	} {
		if got := JoinPrefix(c.base, c.name); got != c.want {
			t.Errorf("JoinPrefix(%q, %q) = %q", c.base, c.name, got)
		}
	}
}
