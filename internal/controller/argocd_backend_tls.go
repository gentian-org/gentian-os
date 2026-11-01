/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"reflect"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Argo CD's server serves TLS, and the Gateway checks it. The internal CA
// signs the Service's name into the Secret Argo CD reads at start and on every
// change, and the Gateway verifies that name against the same Secret's ca.crt.

// argocdServerTLSSecret is the name Argo CD's server looks for.
const argocdServerTLSSecret = "argocd-server-tls"

// The label Argo CD watches its settings Secrets by.
const (
	argocdPartOfLabel = "app.kubernetes.io/part-of"
	argocdPartOfValue = "argocd"
)

// argocdServerHTTPSPort is the Service's TLS port.
const argocdServerHTTPSPort = 443

var backendTLSPolicyGVK = schema.GroupVersionKind{
	Group: "gateway.networking.k8s.io", Version: "v1", Kind: "BackendTLSPolicy",
}

func argocdServerName(namespace string) string {
	return argocdServerServiceName + "." + namespace + ".svc.cluster.local"
}

func buildArgoCDServerCertificate(namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certManagerCertGVK)
	obj.SetName(argocdServerTLSSecret)
	obj.SetNamespace(namespace)
	obj.SetLabels(map[string]string{managedByLabel: managedByValue})
	name := argocdServerName(namespace)
	obj.Object["spec"] = map[string]interface{}{
		"secretName": argocdServerTLSSecret,
		// Argo CD reloads only a Secret labeled as part of it; without this it
		// keeps the certificate it started with.
		"secretTemplate": map[string]interface{}{
			"labels": map[string]interface{}{argocdPartOfLabel: argocdPartOfValue},
		},
		"commonName": name,
		"dnsNames":   []interface{}{name, argocdServerServiceName + "." + namespace + ".svc"},
		// The role signs EC P-256 keys.
		"privateKey": map[string]interface{}{"algorithm": "ECDSA", "size": int64(256), "rotationPolicy": "Always"},
		"usages":     []interface{}{"digital signature", "server auth"},
		"issuerRef":  map[string]interface{}{"name": "gentian-ca", "kind": "ClusterIssuer"},
	}
	return obj
}

func buildArgoCDBackendTLSPolicy(namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(backendTLSPolicyGVK)
	obj.SetName(argocdServerServiceName)
	obj.SetNamespace(namespace)
	obj.SetLabels(map[string]string{managedByLabel: managedByValue})
	obj.Object["spec"] = map[string]interface{}{
		"targetRefs": []interface{}{map[string]interface{}{
			"group": "", "kind": "Service", "name": argocdServerServiceName,
		}},
		"validation": map[string]interface{}{
			"hostname": argocdServerName(namespace),
			"caCertificateRefs": []interface{}{map[string]interface{}{
				"group": "", "kind": "Secret", "name": argocdServerTLSSecret,
			}},
		},
	}
	return obj
}

func applySpecOf(ctx context.Context, c client.Client, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	existing.Object["spec"] = desired.Object["spec"]
	return c.Update(ctx, existing)
}

// ensureArgoCDBackendTLS writes the server's certificate and the Gateway's
// check of it.
func (r *GatewayPlatformReconciler) ensureArgoCDBackendTLS(ctx context.Context) error {
	if err := applySpecOf(ctx, r.Client, buildArgoCDServerCertificate(argocdNamespace)); err != nil {
		return err
	}
	return applySpecOf(ctx, r.Client, buildArgoCDBackendTLSPolicy(argocdNamespace))
}
