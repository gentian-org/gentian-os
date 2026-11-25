/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// EdgeCertificatesGardener is the value of EDGE_CERTIFICATES on a cluster
// whose DNS provider issues the edge's certificates itself: a Gardener shoot
// with its certificate service (kernel/platforms.yaml, certificates:
// gardener). The service solves DNS-01 with the credential the shoot holds for
// the zone, so wildcards are issued where the platform holds none and its
// cert-manager has no DNS-01 issuer. Anything else means cert-manager.
const EdgeCertificatesGardener = "gardener"

var gardenerCertGVK = schema.GroupVersionKind{
	Group:   "cert.gardener.cloud",
	Version: "v1alpha1",
	Kind:    "Certificate",
}

// edgeCertificateGVK is the kind of certificate a source is asked for.
func edgeCertificateGVK(source string) schema.GroupVersionKind {
	if source == EdgeCertificatesGardener {
		return gardenerCertGVK
	}
	return certManagerCertGVK
}

// buildEdgeCertificate asks the cluster's certificate source for one
// certificate naming exactly names, written to secretName beside it.
// clusterIssuer is cert-manager's; Gardener's service uses the shoot's own
// issuer.
func buildEdgeCertificate(source, name, namespace, secretName string, names []string, clusterIssuer string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(edgeCertificateGVK(source))
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if source == EdgeCertificatesGardener {
		// The first name is the subject; the service puts it in the
		// certificate's names too, so it is not listed twice.
		_ = unstructured.SetNestedField(obj.Object, names[0], "spec", "commonName")
		if len(names) > 1 {
			_ = unstructured.SetNestedStringSlice(obj.Object, names[1:], "spec", "dnsNames")
		}
		_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
			"name":      secretName,
			"namespace": namespace,
		}, "spec", "secretRef")
		return obj
	}
	_ = unstructured.SetNestedStringSlice(obj.Object, names, "spec", "dnsNames")
	_ = unstructured.SetNestedField(obj.Object, secretName, "spec", "secretName")
	_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
		"name": clusterIssuer,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
	return obj
}
