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
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/mailer"
)

// NotifyAdministratorsPath is the director's way of telling the cluster's
// administrators something by mail: a release that waits for them, a move
// the platform made by itself. The director holds no credential, not for
// mail and not for Keycloak; the operator holds both already -- the SMTP
// settings it configures every realm with, and the admin client it reads
// groups with -- so it sends on the director's behalf, as it publishes the
// director's notices into the console.
const NotifyAdministratorsPath = "/v1/actions/notify-administrators"

const (
	keycloakSMTPCredentialsSecret = "keycloak-smtp-credentials"
	defaultPlatformAdminGroup     = "gentian:platform:admin"
)

// ErrNoAdministratorAddress is a platform admin group with nobody in it who
// has an address.
var ErrNoAdministratorAddress = errors.New("no administrator of this cluster has a mail address")

// NotifyAdministratorsRequest is what the director sends.
type NotifyAdministratorsRequest struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

// NotifyAdministratorsResult says who was written to.
type NotifyAdministratorsResult struct {
	Recipients []string `json:"recipients"`
}

func (h *HTTPServer) registerAdministratorNotificationRoutes(mux router) {
	mux.HandleFunc("POST "+NotifyAdministratorsPath, h.handleNotifyAdministrators)
}

func (h *HTTPServer) handleNotifyAdministrators(w http.ResponseWriter, r *http.Request) {
	var body NotifyAdministratorsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	res, err := h.Service.NotifyAdministrators(r.Context(), body)
	switch {
	case errors.Is(err, mailer.ErrNotConfigured), errors.Is(err, ErrNoAdministratorAddress):
		writeErr(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, ErrNoClusterClaim):
		writeErr(w, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
	default:
		writeJSON(w, http.StatusAccepted, res)
	}
}

// NotifyAdministrators mails the members of the cluster's platform admin
// group (spec.platformRoles.admin on the Cluster claim) through the
// platform's SMTP settings (keycloak-smtp-credentials, in either mail
// mode). Refused when mail is not configured or nobody has an address.
func (s *Service) NotifyAdministrators(ctx context.Context, req NotifyAdministratorsRequest) (*NotifyAdministratorsResult, error) {
	if strings.TrimSpace(req.Subject) == "" || strings.TrimSpace(req.Text) == "" {
		return nil, fmt.Errorf("a notification needs a subject and a text")
	}
	claim, err := s.clusterClaim(ctx)
	if err != nil {
		return nil, err
	}
	kernelDomain, _, _ := unstructured.NestedString(claim.Object, "spec", "kernelDomain")
	group, _, _ := unstructured.NestedString(claim.Object, "spec", "platformRoles", "admin")
	if group == "" {
		group = defaultPlatformAdminGroup
	}
	secret := &corev1.Secret{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: keycloakSMTPCredentialsSecret, Namespace: layout.Namespace(layout.Authentication)}, secret); err != nil {
		return nil, fmt.Errorf("%w: %v", mailer.ErrNotConfigured, err)
	}
	settings, err := mailer.SettingsFromSecret(secret.Data, kernelDomain)
	if err != nil {
		return nil, err
	}
	realm := strings.TrimSpace(string(secret.Data["kernel_realm"]))
	if realm == "" {
		realm = "kernel"
	}
	members, err := s.groupMembers(ctx, realm, group)
	if err != nil {
		return nil, fmt.Errorf("the members of %s could not be read: %w", group, err)
	}
	to := administratorAddresses(members)
	if len(to) == 0 {
		return nil, fmt.Errorf("%w (group %s of realm %s)", ErrNoAdministratorAddress, group, realm)
	}
	send := s.sendMail
	if send == nil {
		send = mailer.Send
	}
	if err := send(ctx, settings, mailer.Message{To: to, Subject: req.Subject, Text: req.Text}); err != nil {
		return nil, err
	}
	return &NotifyAdministratorsResult{Recipients: to}, nil
}

// groupMembers lists a group's enabled members, through the Keycloak admin
// client the operator already holds; a test sets listGroupMembers.
func (s *Service) groupMembers(ctx context.Context, realm, group string) ([]authz.KeycloakUser, error) {
	if s.listGroupMembers != nil {
		return s.listGroupMembers(ctx, realm, group)
	}
	kc, err := authz.LoadKeycloakAdminClient(ctx, s.client)
	if err != nil {
		return nil, err
	}
	return kc.ListGroupMembers(ctx, realm, group)
}

// administratorAddresses is where the members receive mail: the login
// where it is an address (a tenant realm), else the email field (the
// kernel realm, whose administrator is "administrator"). Sorted, deduplicated.
func administratorAddresses(members []authz.KeycloakUser) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range members {
		if !u.Enabled {
			continue
		}
		addr := ""
		switch {
		case strings.Contains(u.Username, "@"):
			addr = u.Username
		case strings.Contains(u.Email, "@"):
			addr = u.Email
		}
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}
