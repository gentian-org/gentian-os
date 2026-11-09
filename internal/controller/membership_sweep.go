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
	"log/slog"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/membership"
)

// MembershipSweepInterval is how often the sweep runs.
const MembershipSweepInterval = 15 * time.Minute

// MembershipSweep removes, on an interval, the memberships of people who are
// no longer in Keycloak: what a deletion statement does when it arrives, for
// the ones that did not (membership.Receiver.Sweep).
type MembershipSweep struct {
	// Receiver applies the statements Keycloak sends.
	Receiver *membership.Receiver
	// Client reads Keycloak's admin credential.
	Client client.Reader
	// Interval between sweeps; MembershipSweepInterval when zero.
	Interval time.Duration
	Log      *slog.Logger
}

// Start sweeps once a minute after the manager starts, then every Interval,
// until the manager's context is canceled.
func (s *MembershipSweep) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = MembershipSweepInterval
	}
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = interval
		s.sweep(ctx)
	}
}

func (s *MembershipSweep) sweep(ctx context.Context) {
	kc, err := authz.LoadKeycloakAdminClient(ctx, s.Client)
	if err != nil {
		s.Log.ErrorContext(ctx, "membership sweep skipped: no Keycloak admin credential", "error", err.Error())
		return
	}
	if _, err := s.Receiver.Sweep(ctx, keycloakPeople{kc}); err != nil {
		s.Log.ErrorContext(ctx, "membership sweep incomplete", "error", err.Error())
	}
}

// NeedLeaderElection keeps one replica sweeping, the one that also receives
// the statements.
func (s *MembershipSweep) NeedLeaderElection() bool { return true }

// keycloakPeople is membership.People over the admin client.
type keycloakPeople struct{ kc *authz.KeycloakAdminClient }

func (p keycloakPeople) AccountIDs(ctx context.Context, realm string) ([]string, error) {
	accounts, err := p.kc.ListRealmAccounts(ctx, realm)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	return ids, nil
}

func (p keycloakPeople) Exists(ctx context.Context, realm, id string) (bool, error) {
	return p.kc.UserExists(ctx, realm, id)
}
