/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGatewayChecksTheNameOnArgoCDsCertificate(t *testing.T) {
	cert := buildArgoCDServerCertificate("kernel-gitops")
	names, _, _ := unstructured.NestedSlice(cert.Object, "spec", "dnsNames")
	policy := buildArgoCDBackendTLSPolicy("kernel-gitops")
	host, _, _ := unstructured.NestedString(policy.Object, "spec", "validation", "hostname")
	if len(names) == 0 || names[0] != host {
		t.Fatalf("policy checks %q, certificate names %v", host, names)
	}
	secret, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	refs, _, _ := unstructured.NestedSlice(policy.Object, "spec", "validation", "caCertificateRefs")
	if secret != argocdServerTLSSecret || refs[0].(map[string]interface{})["name"] != secret {
		t.Fatalf("policy reads %v, certificate writes %q", refs, secret)
	}
}
