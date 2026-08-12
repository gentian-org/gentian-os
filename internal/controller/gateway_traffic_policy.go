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
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func (r *TenantReconciler) collectTenantIngressIntents(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]ingressIntent, error) {
	return collectTenantIngressIntents(ctx, r.Client, tenant)
}

func buildAppBackendTrafficPolicyObject(
	tenant *gentianov1alpha1.Tenant,
	nsName, appProfile string,
	ingress *gentianov1alpha1.IngressSpec,
) *unstructured.Unstructured {
	spec := backendTrafficPolicySpecFromIngressAnnotations(ingress.Annotations)
	if spec == nil {
		return nil
	}
	attachBackendTrafficPolicyTarget(spec, appHTTPRouteName(tenant.Name, appProfile))

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(backendTrafficPolicyGVK)
	obj.SetName(appBackendTrafficPolicyName(tenant.Name, appProfile))
	obj.SetNamespace(nsName)
	obj.SetLabels(map[string]string{
		tenantLabel:           tenant.Name,
		appLabel:              appProfile,
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentApp,
	})
	_ = unstructured.SetNestedField(obj.Object, spec, "spec")
	return obj
}

func buildTenantEscapedSlashesClientTrafficPolicyObject(tenant *gentianov1alpha1.Tenant, nsName string) *unstructured.Unstructured {
	policySpec := escapedSlashesKeepUnchangedClientTrafficPolicySpec()
	policySpec["targetRefs"] = []interface{}{
		map[string]interface{}{
			"group":       gatewayv1.GroupName,
			"kind":        "Gateway",
			"name":        tenantGatewayName(tenant.Name),
			"sectionName": wildcardListenerName,
		},
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(clientTrafficPolicyGVK)
	obj.SetName(tenantEscapedSlashesClientTrafficPolicyName(tenant.Name))
	obj.SetNamespace(nsName)
	obj.SetLabels(map[string]string{
		tenantLabel:           tenant.Name,
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentApp,
	})
	_ = unstructured.SetNestedField(obj.Object, policySpec, "spec")
	return obj
}

func backendTrafficPolicySpecFromIngressAnnotations(annotations map[string]string) map[string]interface{} {
	if len(annotations) == 0 {
		return nil
	}
	spec := map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
			},
		},
	}
	var timeout map[string]interface{}
	if d := gatewayDurationAnnotation(annotations, gentianov1alpha1.AnnotationIngressGatewayRequestTimeout); d != "" {
		timeout = map[string]interface{}{"http": map[string]interface{}{"requestTimeout": d}}
	}
	if d := gatewayDurationAnnotation(annotations, gentianov1alpha1.AnnotationIngressGatewayResponseTimeout); d != "" {
		if timeout == nil {
			timeout = map[string]interface{}{}
		}
		http, _ := timeout["http"].(map[string]interface{})
		if http == nil {
			http = map[string]interface{}{}
			timeout["http"] = http
		}
		http["responseTimeout"] = d
	}
	if timeout != nil {
		spec["timeout"] = timeout
	}
	if body := annotations[gentianov1alpha1.AnnotationIngressGatewayBufferLimit]; body != "" {
		spec["connection"] = map[string]interface{}{
			"bufferLimit": body,
		}
	}
	if len(spec) == 1 {
		return nil
	}
	return spec
}

func gatewayDurationAnnotation(annotations map[string]string, key string) string {
	raw := strings.TrimSpace(annotations[key])
	if raw == "" {
		return ""
	}
	if strings.HasSuffix(raw, "s") || strings.HasSuffix(raw, "m") || strings.HasSuffix(raw, "h") {
		return raw
	}
	if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
		return fmt.Sprintf("%ds", sec)
	}
	return raw
}

func attachBackendTrafficPolicyTarget(spec map[string]interface{}, routeName string) {
	refs, ok := spec["targetRefs"].([]interface{})
	if !ok || len(refs) == 0 {
		return
	}
	ref, ok := refs[0].(map[string]interface{})
	if !ok {
		return
	}
	ref["name"] = routeName
}
