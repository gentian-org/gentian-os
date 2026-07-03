/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var referenceGrantGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupName,
	Version: "v1beta1",
	Kind:    "ReferenceGrant",
}

type referenceGrantIntent struct {
	namespace string
	name      string
	spec      map[string]interface{}
}

// tenantKernelGatewayReferenceGrantIntents allows tenant HTTPRoutes to attach to
// kernel-public-gateway and lets that Gateway terminate TLS with the tenant wildcard certificate.
func tenantKernelGatewayReferenceGrantIntents(tenant *gentianov1alpha1.Tenant) []referenceGrantIntent {
	nsName := tenantNamespaceName(tenant)
	return []referenceGrantIntent{
		{
			namespace: servicesNamespace,
			name:      "allow-tenant-routes-" + tenant.Name,
			spec: map[string]interface{}{
				"from": []interface{}{
					map[string]interface{}{
						"group":     gatewayv1.GroupName,
						"kind":      "HTTPRoute",
						"namespace": nsName,
					},
				},
				"to": []interface{}{
					map[string]interface{}{
						"group": gatewayv1.GroupName,
						"kind":  "Gateway",
						"name":  KernelPublicGatewayName,
					},
				},
			},
		},
		{
			namespace: nsName,
			name:      "allow-kernel-gateway-tls",
			spec: map[string]interface{}{
				"from": []interface{}{
					map[string]interface{}{
						"group":     gatewayv1.GroupName,
						"kind":      "Gateway",
						"namespace": servicesNamespace,
					},
				},
				"to": []interface{}{
					map[string]interface{}{
						"group": "",
						"kind":  "Secret",
						"name":  tenantWildcardSecretName(tenant.Name),
					},
				},
			},
		},
	}
}

func buildReferenceGrantObject(namespace, name string, spec map[string]interface{}, labels map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(referenceGrantGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.SetLabels(labels)
	_ = unstructured.SetNestedField(obj.Object, spec, "spec")
	return obj
}

func buildTenantReferenceGrantObjects(tenant *gentianov1alpha1.Tenant) []client.Object {
	labels := map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}
	var objects []client.Object
	for _, intent := range tenantKernelGatewayReferenceGrantIntents(tenant) {
		objects = append(objects, buildReferenceGrantObject(intent.namespace, intent.name, intent.spec, labels))
	}
	return objects
}

func deleteTenantReferenceGrants(ctx context.Context, c client.Client, tenant *gentianov1alpha1.Tenant) error {
	for _, intent := range tenantKernelGatewayReferenceGrantIntents(tenant) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(referenceGrantGVK)
		obj.SetName(intent.name)
		obj.SetNamespace(intent.namespace)
		if err := c.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ReferenceGrant %s/%s: %w", intent.namespace, intent.name, err)
		}
	}
	return nil
}
