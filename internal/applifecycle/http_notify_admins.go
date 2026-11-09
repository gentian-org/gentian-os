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
	"github.com/gentian-org/gentian-os/internal/releaseindex"
)

// NotifyAdministratorsPath is the director's way of telling the cluster's
// administrators something by mail: a release that waits for them, a move
// the platform made by itself. The director holds no credential, not for
// mail and not for Keycloak; the operator holds both already -- the SMTP
// settings it configures every realm with, and the admin client it reads
// groups with -- so it sends on the director's behalf, as it publishes the
// director's notices into the console.
//
// The director names an event, never the words: each event's subject and
// text are fixed here, and every value in them is the operator's own -- a
// release number and its notes as the release index has them, the cluster's
// domain from its claim. A caller's string is a key to look up, and what it
// does not match is refused, so nothing a request carries is written into a
// mail. A new kind of mail is a new entry in adminMails.
const NotifyAdministratorsPath = "/v1/actions/notify-administrators"

const (
	keycloakSMTPCredentialsSecret = "keycloak-smtp-credentials"
	defaultPlatformAdminGroup     = "gentian:platform:admin"
)

// The events the administrators are mailed on.
const (
	// EventReleasePending: Release is out and the cluster's mode does not
	// take it by itself; From is what the cluster runs.
	EventReleasePending = "release-pending"
	// EventReleaseMoved: the cluster moves from From to Release by its
	// autoUpgrade Mode.
	EventReleaseMoved = "release-moved"
)

var (
	// ErrNoAdministratorAddress is a platform admin group with nobody in
	// it who has an address.
	ErrNoAdministratorAddress = errors.New("no administrator of this cluster has a mail address")
	// ErrNotification is a request that names no known event, or a
	// release the index does not list.
	ErrNotification = errors.New("not a notification the operator sends")
	// ErrNoReleaseIndex is a cluster without a release index to take the
	// numbers from, an air-gapped one.
	ErrNoReleaseIndex = errors.New("the release index is not available")
)

// NotifyAdministratorsRequest is what the director sends: an event and the
// facts that pick its values. None of them is written into the mail as sent.
type NotifyAdministratorsRequest struct {
	Event   string `json:"event"`
	Release string `json:"release,omitempty"`
	From    string `json:"from,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// NotifyAdministratorsResult says who was written to.
type NotifyAdministratorsResult struct {
	Recipients []string `json:"recipients"`
}

// mailFacts are the values a mail is written with, all of them the
// operator's: the cluster's domain from its claim, releases from the index.
type mailFacts struct {
	Cluster string
	Release releaseindex.Release
	// From is the release the cluster ran, the zero Release when the index
	// does not list it.
	From releaseindex.Release
	Mode releaseindex.Mode
}

// adminMail writes one event's subject and text.
type adminMail func(f mailFacts) (subject, text string)

var adminMails = map[string]adminMail{
	EventReleasePending: func(f mailFacts) (string, string) {
		subject := fmt.Sprintf("Gentian OS %s waits for cluster %s", f.Release.Version, f.Cluster)
		text := fmt.Sprintf("Release %s of Gentian OS is out. The cluster %s runs %s, and its autoUpgrade mode does not take %s by itself.\n\n"+
			"To move the cluster:\n  kubectl gentian release set %s\nor take it in the administration console, under Release.\n",
			f.Release.Version, f.Cluster, runningRelease(f.From), f.Release.Version, f.Release.Version)
		if f.Release.Notes != "" {
			text += "\nRelease notes: " + f.Release.Notes + "\n"
		}
		return subject, text
	},
	EventReleaseMoved: func(f mailFacts) (string, string) {
		subject := fmt.Sprintf("Gentian OS: cluster %s moves to %s", f.Cluster, f.Release.Version)
		text := fmt.Sprintf("The cluster %s was on %s and moves to %s now, by its autoUpgrade mode %s inside the maintenance window.\n\n"+
			"The operator applies the release; follow it with:\n  kubectl gentian release show\n",
			f.Cluster, runningRelease(f.From), f.Release.Version, f.Mode)
		if f.Release.Notes != "" {
			text += "\nRelease notes: " + f.Release.Notes + "\n"
		}
		return subject, text
	},
}

func runningRelease(r releaseindex.Release) string {
	if r.Version == "" {
		return "an earlier release"
	}
	return r.Version
}

// autoUpgradeMode is the mode a request names, as one of the operator's
// own constants.
func autoUpgradeMode(name string) (releaseindex.Mode, bool) {
	for _, m := range []releaseindex.Mode{releaseindex.ModeNone, releaseindex.ModePatch, releaseindex.ModeMinor} {
		if string(m) == name {
			return m, true
		}
	}
	return "", false
}

func (h *HTTPServer) registerAdministratorNotificationRoutes(mux router) {
	mux.HandleFunc("POST "+NotifyAdministratorsPath, h.handleNotifyAdministrators)
}

func (h *HTTPServer) handleNotifyAdministrators(w http.ResponseWriter, r *http.Request) {
	var body NotifyAdministratorsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	res, err := h.Service.NotifyAdministrators(r.Context(), body)
	switch {
	case errors.Is(err, ErrNotification):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, mailer.ErrNotConfigured), errors.Is(err, ErrNoAdministratorAddress), errors.Is(err, ErrNoReleaseIndex):
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
	write, ok := adminMails[req.Event]
	if !ok {
		return nil, fmt.Errorf("%w: no event %q", ErrNotification, req.Event)
	}
	var mode releaseindex.Mode
	if req.Event == EventReleaseMoved {
		if mode, ok = autoUpgradeMode(req.Mode); !ok {
			return nil, fmt.Errorf("%w: no autoUpgrade mode %q", ErrNotification, req.Mode)
		}
	}
	idx, err := s.releaseIndex(ctx)
	if err != nil {
		return nil, err
	}
	release, ok := idx.Find(req.Release)
	if !ok {
		return nil, fmt.Errorf("%w: the release index lists no %q", ErrNotification, req.Release)
	}
	from, _ := idx.Find(req.From)
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
	cluster := kernelDomain
	if cluster == "" {
		cluster = claim.GetName()
	}
	subject, text := write(mailFacts{Cluster: cluster, Release: release, From: from, Mode: mode})
	send := s.sendMail
	if send == nil {
		send = mailer.Send
	}
	if err := send(ctx, settings, mailer.Message{To: to, Subject: subject, Text: text}); err != nil {
		return nil, err
	}
	return &NotifyAdministratorsResult{Recipients: to}, nil
}

// releaseIndex is the platform's release index from the configured feed; a
// test sets fetchReleaseIndex.
func (s *Service) releaseIndex(ctx context.Context) (releaseindex.Index, error) {
	if s.fetchReleaseIndex != nil {
		return s.fetchReleaseIndex(ctx)
	}
	if s.opts.ReleaseFeed == "" {
		return releaseindex.Index{}, ErrNoReleaseIndex
	}
	idx, err := releaseindex.Fetch(ctx, nil, s.opts.ReleaseFeed)
	if err != nil {
		return releaseindex.Index{}, fmt.Errorf("%w: %v", ErrNoReleaseIndex, err)
	}
	return idx, nil
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
