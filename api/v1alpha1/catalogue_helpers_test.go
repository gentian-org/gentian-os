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

package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestProfileGatewayAnnotations(t *testing.T) {
	p := &v1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				v1alpha1.AnnotationProfileGatewayRootRedirect: "/app/",
				v1alpha1.AnnotationProfileGatewayAPIBackends:  `[{"pathPrefix":"/app/api","serviceName":"demo-api"}]`,
			},
		},
	}
	if v1alpha1.ProfileGatewayRootRedirect(p) != "/app/" {
		t.Fatalf("root redirect: %q", v1alpha1.ProfileGatewayRootRedirect(p))
	}
	backends, err := v1alpha1.ProfileGatewayAPIBackends(p)
	if err != nil || len(backends) != 1 || backends[0].ServiceName != "demo-api" {
		t.Fatalf("api backends: %v, %v", backends, err)
	}
}

func TestProfileOIDCDefaultRedirectURIs(t *testing.T) {
	p := &v1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				v1alpha1.AnnotationProfileOIDCDefaultRedirectURIs: `["https://demo.${TENANT_DOMAIN}/oidc/callback"]`,
			},
		},
	}
	uris, err := v1alpha1.ProfileOIDCDefaultRedirectURIs(p)
	if err != nil || len(uris) != 1 || uris[0] != "https://demo.${TENANT_DOMAIN}/oidc/callback" {
		t.Fatalf("oidc defaults: %v, %v", uris, err)
	}
}
