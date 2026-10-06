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

// ProfileRequiresEntitlement reports whether an entitlement must be redeemed
// before this entry may be activated.
//
// An annotation, not spec.license. AD-3 moves the licence to the store's
// listing, outside the cluster, because it is presentation — but whether an
// entry NEEDS PAYING FOR is an authorization question, and the thing that
// enforces it runs here. A gate reading a field that has left is a gate that
// always opens.
func ProfileRequiresEntitlement(p *ComponentProfile) bool {
	if p == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(p.Annotations[AnnotationProfileRequiresEntitlement]), "true")
}

// GatewayAPIBackend is one extra HTTPRoute rule: path prefix → Kubernetes Service.
type GatewayAPIBackend struct {
	PathPrefix  string `json:"pathPrefix"`
	ServiceName string `json:"serviceName"`
	Port        int32  `json:"port,omitempty"`
}

// ProfileGatewayRootRedirect returns gentianos.io/gateway-root-redirect when set.
func ProfileGatewayRootRedirect(p *ComponentProfile) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Annotations[AnnotationProfileGatewayRootRedirect])
}

// ProfileGatewayAPIBackends parses gentianos.io/gateway-api-backends JSON.
func ProfileGatewayAPIBackends(p *ComponentProfile) ([]GatewayAPIBackend, error) {
	if p == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(p.Annotations[AnnotationProfileGatewayAPIBackends])
	if raw == "" {
		return nil, nil
	}
	var backends []GatewayAPIBackend
	if err := json.Unmarshal([]byte(raw), &backends); err != nil {
		return nil, err
	}
	return backends, nil
}

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

// GatewayFrameAncestorsSpec is the JSON shape for gentianos.io/gateway-frame-ancestors.
type GatewayFrameAncestorsSpec struct {
	Mode    string   `json:"mode"`
	Origins []string `json:"origins"`
}

// GatewayFrameAncestors parses gentianos.io/gateway-frame-ancestors on one
// exposure's annotations.
func GatewayFrameAncestors(annotations map[string]string) (*GatewayFrameAncestorsSpec, error) {
	if len(annotations) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(annotations[AnnotationIngressGatewayFrameAncestors])
	if raw == "" {
		return nil, nil
	}
	var spec GatewayFrameAncestorsSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// GatewayEscapedSlashesAction returns gentianos.io/gateway-escaped-slashes-action
// on one exposure's annotations, when set.
func GatewayEscapedSlashesAction(annotations map[string]string) string {
	if len(annotations) == 0 {
		return ""
	}
	return strings.TrimSpace(annotations[AnnotationIngressGatewayEscapedSlashesAction])
}

// ProfileKernelEgressNamespaces parses gentianos.io/kernel-egress-namespaces on an AppProfile.
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
