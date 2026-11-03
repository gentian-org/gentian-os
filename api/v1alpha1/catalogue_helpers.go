/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import (
	"encoding/json"
	"strings"
)

// ProfileOIDCDefaultRedirectURIs parses gentianos.io/oidc-default-redirect-uris JSON.
func ProfileOIDCDefaultRedirectURIs(p *ComponentProfile) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(p.Annotations[AnnotationProfileOIDCDefaultRedirectURIs])
	if raw == "" {
		return nil, nil
	}
	var uris []string
	if err := json.Unmarshal([]byte(raw), &uris); err != nil {
		return nil, err
	}
	return uris, nil
}

// GatewayEscapedSlashesAction returns gentianos.io/gateway-escaped-slashes-action
// on one exposure's annotations, when set.
func GatewayEscapedSlashesAction(annotations map[string]string) string {
	if len(annotations) == 0 {
		return ""
	}
	return strings.TrimSpace(annotations[AnnotationIngressGatewayEscapedSlashesAction])
}

// ProfileKernelEgressNamespaces parses gentianos.io/kernel-egress-namespaces on a ComponentProfile.
func ProfileKernelEgressNamespaces(p *ComponentProfile) []string {
	if p == nil || len(p.Annotations) == 0 {
		return nil
	}
	raw := strings.TrimSpace(p.Annotations[AnnotationProfileKernelEgressNamespaces])
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if ns := strings.TrimSpace(part); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}
