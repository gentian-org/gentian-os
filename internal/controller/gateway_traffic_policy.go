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
	"fmt"
	"strconv"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func (r *TenantReconciler) collectTenantIngressIntents(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]ingressIntent, error) {
	return collectTenantIngressIntents(ctx, r.Client, tenant)
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
