/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The DNS check's answer, as written; before its first pass an empty one.
func TestTheDomainsRouteAnswersTheDNSCheck(t *testing.T) {
	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace = dnsStatusConfigMapName, "kernel-control"
	cm.Data = map[string]string{dnsStatusKey: `{"checkedAt":"2026-11-10T00:00:00Z","domains":[{"domain":"gentian.org","tenant":"gentian","records":[]}]}`}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).Build()
	h := &HTTPServer{Service: &Service{client: c, opts: Options{OperatorNamespace: "kernel-control"}}}
	rec := httptest.NewRecorder()
	h.handleDomains(rec, httptest.NewRequest(http.MethodGet, DomainsPath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"domain":"gentian.org"`) {
		t.Fatalf("GET %s = %d %s", DomainsPath, rec.Code, rec.Body.String())
	}

	empty := &HTTPServer{Service: &Service{
		client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build(),
		opts:   Options{OperatorNamespace: "kernel-control"},
	}}
	rec = httptest.NewRecorder()
	empty.handleDomains(rec, httptest.NewRequest(http.MethodGet, DomainsPath, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"checkedAt":"","domains":[]}` {
		t.Fatalf("before the first check = %d %s", rec.Code, rec.Body.String())
	}
}
