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
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/authz"
	rights "github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/mailer"
	"github.com/gentian-org/gentian-os/internal/releaseindex"
	"github.com/gentian-org/gentian-os/internal/usage"
)

const platformAppAnnotation = "gentianos.io/platform-app"

// Service answers what the cluster holds of a tenant's apps, resources and
// backups, and carries out the commands of the operator's listener. It writes
// nothing to git: what a tenant is meant to have is the director's commit.
type Service struct {
	client    client.Client
	clientset kubernetes.Interface
	opts      Options
	// actualSource reads live consumption for the resources API. Nil when the
	// cluster has no metrics source, which is a supported configuration: the
	// ceiling and the committed usage under it come from the API server, and
	// those are the figures a plan is chosen and billed on.
	actualSource usage.ActualSource
	// appLocks serializes lifecycle operations per (tenant, profile) — see lockApp.
	appLocks sync.Map
	// residueMu lets one removal of catalog residue run at a time: each
	// works the list out and then deletes on it.
	residueMu sync.Mutex
	// refreshMu guards when Argo CD was last asked to read the deployments
	// repository, and whether one more refresh is owed (http_refresh.go).
	refreshMu   sync.Mutex
	refreshLast time.Time
	refreshOwed bool

	// vault is where apps' credentials are stored. Nil when the operator was
	// given no vault to talk to; a purge then fails at its credentials step
	// rather than report them destroyed.
	vault CredentialStore
	// sendMail and listGroupMembers are how NotifyAdministrators reaches
	// the relay and Keycloak; nil is the real thing, a test sets them.
	sendMail         func(ctx context.Context, s mailer.Settings, m mailer.Message) error
	listGroupMembers func(ctx context.Context, realm, group string) ([]authz.KeycloakUser, error)
	// fetchReleaseIndex, when set, is the release index in place of the
	// configured feed. Tests set it.
	fetchReleaseIndex func(ctx context.Context) (releaseindex.Index, error)
	// groups, when set, is the identity provider to use in place of the one
	// the keycloak-admin Secret names. Tests set it.
	groups AccessGroups
	// models, when set, is the model gateway to use in place of the one the
	// cluster's admin-key Secret names. Tests set it.
	models func(ctx context.Context) (ModelKeys, bool, error)
	// authorization is the arbiter's platform path, nil without one.
	authorization AuthorizationStores
	// Rights is the rights store, for what it still holds on an uninstalled
	// app: who was given a role on it, which a purge removes. Nil on a
	// cluster that runs no rights store, where there is nothing to remove.
	Rights rights.Store
}

// AuthorizationStores is the part of the arbiter's platform path a purge
// uses. *arbiter.PlatformClient is one.
type AuthorizationStores interface {
	Purge(ctx context.Context, tenant, component string) error
}

// accessGroups is the identity provider, as a purge uses it for an app's
// clients and scopes, with the administrator credential the operator holds
// beside it.
func (s *Service) accessGroups(ctx context.Context) (AccessGroups, error) {
	if s.groups != nil {
		return s.groups, nil
	}
	kc, err := authz.LoadKeycloakAdminClient(ctx, s.client)
	if err != nil {
		return nil, fmt.Errorf("load keycloak admin credentials: %w", err)
	}
	return kc, nil
}

// tryLockApp is lockApp for a request that must not queue: it reports false
// when another operation on the app is running, and the caller says so.
func (s *Service) tryLockApp(tenant, profile string) (func(), bool) {
	v, _ := s.appLocks.LoadOrStore(tenant+"/"+profile, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// lockApp blocks until no other lifecycle operation is running for this app,
// and returns the release function.
//
// Purging deletes what an app left behind on the assumption that nothing is
// putting it back, and provisioning writes group memberships for an app that
// is there. Run two of them against the same app at once and they interleave
// badly. Serializing per app costs nothing when they are for different apps,
// which is the normal case.
//
// In-process is sufficient: the operator runs a single replica, and with
// leader election on only one manager is active.
func (s *Service) lockApp(tenant, profile string) func() {
	v, _ := s.appLocks.LoadOrStore(tenant+"/"+profile, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// NewService constructs a lifecycle service.
func NewService(c client.Client, cfg *rest.Config, opts Options) (*Service, error) {
	if opts.OperatorNamespace == "" {
		opts.OperatorNamespace = layout.Namespace(layout.Control)
	}
	if opts.OperatorSA == "" {
		opts.OperatorSA = "gentian-os"
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	svc := &Service{
		client:    c,
		clientset: cs,
		opts:      opts,
	}
	// Assigned only when there is one: an interface holding a nil pointer is
	// not nil, and the purge's check for "no vault" would pass it.
	if opts.Vault != nil {
		svc.vault = opts.Vault
	}
	if opts.Authorization != nil {
		svc.authorization = opts.Authorization
	}
	// Constructed rather than probed: metrics.k8s.io may be absent, and
	// discovering that at start-up would make the operator's readiness depend
	// on an optional add-on. A source that cannot answer reports so per call,
	// where the caller can be told which series is missing and why.
	if opts.MetricsEnabled {
		src, err := usage.NewMetricsAPISource(cfg)
		if err != nil {
			return nil, fmt.Errorf("build metrics source: %w", err)
		}
		svc.actualSource = src
	}
	return svc, nil
}
