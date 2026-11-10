/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DomainsPath is where the director reads the DNS records each served domain
// needs and whether the internet has them. The operator's DNS check writes
// them (internal/controller/dns_domain_status.go); the director holds no
// cluster credential, so they reach it through here.
const DomainsPath = "/v1/domains"

// The ConfigMap the DNS check writes, in the operator's namespace. Named here
// as well as there because this package does not import the controllers.
const (
	dnsStatusConfigMapName = "gentian-dns-status"
	dnsStatusKey           = "status.json"
)

func (h *HTTPServer) handleDomains(w http.ResponseWriter, r *http.Request) {
	raw, err := h.Service.Domains(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// Domains is the DNS check's last answer as it wrote it. Before its first
// pass there is none, and the answer says so: no time, no domains.
func (s *Service) Domains(ctx context.Context) (json.RawMessage, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: s.opts.OperatorNamespace, Name: dnsStatusConfigMapName}
	if err := s.client.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return json.RawMessage(`{"checkedAt":"","domains":[]}`), nil
		}
		return nil, fmt.Errorf("read the DNS status: %w", err)
	}
	raw := cm.Data[dnsStatusKey]
	if !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("the DNS status in %s/%s is not JSON", key.Namespace, key.Name)
	}
	return json.RawMessage(raw), nil
}
