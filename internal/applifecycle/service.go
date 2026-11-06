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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
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
	// groups, when set, is the identity provider to use in place of the one
	// the keycloak-admin Secret names. Tests set it.
	groups AccessGroups
	// models, when set, is the model gateway to use in place of the one the
	// cluster's admin-key Secret names. Tests set it.
	models func(ctx context.Context) (ModelKeys, bool, error)
	// authorization is the arbiter's platform path, nil without one.
	authorization AuthorizationStores
}

// AuthorizationStores is the part of the arbiter's platform path a purge
// uses. *arbiter.PlatformClient is one.
type AuthorizationStores interface {
	Purge(ctx context.Context, tenant, component string) error
}

// accessGroups is the identity provider's groups, with the administrator
// credential the operator holds beside it.
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

func (s *Service) provisionAppGroupUsers(ctx context.Context, tenant *gentianov1alpha1.Tenant, profileName string) error {
	// The group's name carries the tenant's; the realm it is in is the
	// tenant's own, which need not be called the same.
	tenantName, realm := tenant.Name, keycloak.RealmName(tenant)
	// ComponentProfile: AD-4 leaves one catalog kind, and this asked for
	// the other one long after the catalog stopped shipping it, so every
	// call failed with a NotFound and no app group ever got its attributes.
	profile := &gentianov1alpha1.ComponentProfile{}
	err := s.client.Get(ctx, client.ObjectKey{Name: profileName}, profile)
	if err != nil {
		return fmt.Errorf("failed to get ComponentProfile %s: %w", profileName, err)
	}

	// A malformed annotation gives the group no declared attributes.
	var attrs map[string][]string
	if val, ok := profile.GetAnnotations()[keycloak.GroupAttributesAnnotation]; ok {
		attrs, _ = keycloak.ParseGroupAttributes(val)
	}

	kc, err := authz.LoadKeycloakAdminClient(ctx, s.client)
	if err != nil {
		return fmt.Errorf("load keycloak admin credentials: %w", err)
	}

	fullGroupName := keycloak.TenantAppGroup(tenantName, profileName)

	// Provisioning is what separates Install from Provision, and the difference
	// outlives this call: the tenant admin adding a user later should find this
	// app already ticked, while one that was merely installed starts unticked.
	// Recorded on the group because that is what the admin console reads, and
	// because it is per app and per tenant exactly as the grant is.
	if attrs == nil {
		attrs = map[string][]string{}
	}
	attrs[keycloak.DefaultGrantAttribute] = []string{"true"}
	attrs[keycloak.PlatformMarkerAttribute] = []string{keycloak.PlatformMarkerValue}

	groupID, err := kc.EnsureGroup(ctx, realm, fullGroupName, attrs)
	if err != nil {
		return fmt.Errorf("ensure keycloak group %s: %w", fullGroupName, err)
	}
	if groupID == "" {
		return fmt.Errorf("group %s ID not found", fullGroupName)
	}

	// Everyone in the realm -- unless people sign up to it themselves, when
	// the realm holds accounts that are nobody's members and a grant to
	// every member must not reach them.
	var users []authz.KeycloakUser
	if tenant.SignUpEnabled() {
		users, err = kc.ListGroupMembers(ctx, realm, keycloak.TenantMembersGroup(tenantName))
	} else {
		users, err = kc.ListRealmUsers(ctx, realm)
	}
	if err != nil {
		return fmt.Errorf("list keycloak users in realm %s: %w", realm, err)
	}

	// An installed app is for the tenant's people, and an administrator account
	// is given one by hand if it needs it. A placed app keeps everybody: several
	// are what administrators work with.
	skip := map[string]bool{}
	if profile.PlacementOf() == nil {
		admins, err := kc.ListGroupMembers(ctx, realm, keycloak.TenantAdminsGroup(tenantName))
		if err != nil {
			return fmt.Errorf("list the administrators of realm %s: %w", realm, err)
		}
		for _, admin := range admins {
			skip[admin.ID] = true
		}
	}

	for _, u := range users {
		if skip[u.ID] {
			continue
		}
		if err := kc.AddUserToGroup(ctx, realm, u.ID, groupID); err != nil {
			return fmt.Errorf("failed to add user %s to group %s: %w", u.Username, fullGroupName, err)
		}
	}

	return nil
}

// GrantAppByDefault is ProvisionApp for a reconciler: what an app entry
// declaring defaultGrant asks for, done when it can be.
//
// It waits for the app's group rather than making it. The group is created
// with the tenant's identity, from what the profile declares, and a grant
// that ran ahead of that would hand everybody a group nothing yet reads.
// granted is false, with no error, while the group is not there; the caller
// tries again.
//
// The realm is the tenant's own (keycloak.RealmName).
func (s *Service) GrantAppByDefault(ctx context.Context, tenantName, profile string) (granted bool, err error) {
	defer s.lockApp(tenantName, profile)()
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return false, err
	}

	kc, err := authz.LoadKeycloakAdminClient(ctx, s.client)
	if err != nil {
		return false, fmt.Errorf("load keycloak admin credentials: %w", err)
	}
	exists, err := kc.GroupExists(ctx, keycloak.RealmName(tenant), keycloak.TenantAppGroup(tenantName, profile))
	if err != nil {
		return false, fmt.Errorf("look up the app's group: %w", err)
	}
	if !exists {
		return false, nil
	}
	if err := s.provisionAppGroupUsers(ctx, tenant, profile); err != nil {
		return false, err
	}
	return true, nil
}
