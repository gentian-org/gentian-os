/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// The registrar's administrative credential in one realm.
//
// The registrar speaks for Keycloak on a caller's behalf (S7A.17) and holds one
// credential PER REALM rather than one that can reach every realm. That is the
// whole security argument: the registrar authorises per tenant, so a missed
// check should not be able to reach a realm the caller has nothing to do with,
// and a credential that only exists for one realm makes that structural rather
// than policed.
//
// This is the operator's work, not the registrar's, and not Crossplane's.
//
//   - Not the registrar's: it would have to hold a credential that can create
//     administrative clients in order to create the one it is allowed to use,
//     which is the privilege the split exists to avoid.
//   - Not Crossplane's: provider-keycloak's ClientServiceAccountRole needs the
//     INTERNAL id of the realm-management client that provides the role, and
//     that id is generated when the realm is created. Terraform reads it with a
//     data source; the provider has none, and there is nothing to seed an
//     external-name with. Looking it up is a runtime step, so it belongs in a
//     reconciler.

// RegistrarClientID is the client the registrar authenticates as. Taken from
// the registrar's own package rather than spelled again here: the operator
// writes this credential and the registrar asks for it by name, and a name the
// two disagree about produces a realm the registrar cannot speak for with no
// error anywhere to say why.
const RegistrarClientID = identity.ClientID

// registrarRealmRoles are the realm-management roles the registrar is granted,
// and the list is the security statement: everything it can do in a realm is
// here, and anything else is a change somebody has to make deliberately.
//
//   - view-users, query-users, query-groups — the people and group screens.
//   - manage-users — inviting somebody and changing a membership.
//   - manage-realm — the password policy, which is a realm setting. This is
//     the broad one, and it is here because Keycloak has no narrower role for
//     a realm setting. If that becomes uncomfortable, the policy screen moves
//     to a second client and this list loses its last broad entry.
//
// Deliberately NOT here: manage-clients, manage-identity-providers,
// manage-authorization, impersonation, view-events. The registrar configures no
// clients (compositions do), brokers no identities, and reads events through
// its own read path rather than by holding the role that can also clear them.
var registrarRealmRoles = []string{
	"view-users",
	"query-users",
	"query-groups",
	"manage-users",
	"manage-realm",
}

// RegistrarRealmRoles is the list, for a caller that wants to report or assert
// it rather than re-spell it.
func RegistrarRealmRoles() []string {
	out := append([]string(nil), registrarRealmRoles...)
	sort.Strings(out)
	return out
}

type keycloakClientRecord struct {
	ID                     string `json:"id"`
	ClientID               string `json:"clientId"`
	Name                   string `json:"name"`
	Enabled                bool   `json:"enabled"`
	PublicClient           bool   `json:"publicClient"`
	ServiceAccountsEnabled bool   `json:"serviceAccountsEnabled"`
	StandardFlowEnabled    bool   `json:"standardFlowEnabled"`
	Secret                 string `json:"secret"`
}

type keycloakRoleRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EnsureRegistrarRealmClient makes the registrar's credential for one realm
// exist, and returns its secret.
//
// Idempotent: it is what a reconciler calls on every pass. It does not rotate
// the secret -- a rotation is a deliberate act with a window in which the
// registrar must be handed the new value before the old one stops working, and
// doing it silently on a reconcile would sign people out of a screen halfway
// through using it.
func (c *KeycloakAdminClient) EnsureRegistrarRealmClient(ctx context.Context, realm string) (string, error) {
	if realm == "" || strings.ContainsAny(realm, "/?#") {
		return "", fmt.Errorf("not a realm name: %q", realm)
	}
	token, err := c.adminToken(ctx)
	if err != nil {
		return "", err
	}

	client, err := c.findClient(ctx, token, realm, RegistrarClientID)
	if err != nil {
		return "", err
	}
	if client == nil {
		if err := c.createRegistrarClient(ctx, token, realm); err != nil {
			return "", err
		}
		client, err = c.findClient(ctx, token, realm, RegistrarClientID)
		if err != nil {
			return "", err
		}
		if client == nil {
			return "", fmt.Errorf("keycloak realm %s: created %s and cannot find it", realm, RegistrarClientID)
		}
	}

	// The flags are asserted on every pass, not only at creation. A client
	// somebody turned into a public one, or whose service account was
	// disabled, is a credential that silently stops working -- and in the
	// public case, one that stops being a credential at all.
	if err := c.assertRegistrarClientShape(ctx, token, realm, client); err != nil {
		return "", err
	}
	if err := c.grantRegistrarRoles(ctx, token, realm, client.ID); err != nil {
		return "", err
	}
	return c.clientSecret(ctx, token, realm, client.ID)
}

// findClient looks one client up by its clientId. Keycloak's list endpoint
// takes clientId as an exact filter, so this is one call rather than a page
// walk.
func (c *KeycloakAdminClient) findClient(ctx context.Context, token, realm, clientID string) (*keycloakClientRecord, error) {
	var found []keycloakClientRecord
	path := fmt.Sprintf("/admin/realms/%s/clients?clientId=%s",
		url.PathEscape(realm), url.QueryEscape(clientID))
	if err := c.getAdminJSON(ctx, token, path, &found); err != nil {
		return nil, err
	}
	for i := range found {
		if found[i].ClientID == clientID {
			return &found[i], nil
		}
	}
	return nil, nil
}

func (c *KeycloakAdminClient) createRegistrarClient(ctx context.Context, token, realm string) error {
	body := map[string]any{
		"clientId":    RegistrarClientID,
		"name":        "Gentian registrar (administrative)",
		"description": "The registrar acts for a caller the platform has already authorised. Its authority is the caller's; this credential is only how it reaches this realm.",
		"enabled":     true,
		// Confidential with a service account, and nothing else. No browser
		// flow, no direct grant: nobody signs in as this, and a credential
		// that could be used interactively is one that can be phished.
		"publicClient":              false,
		"serviceAccountsEnabled":    true,
		"standardFlowEnabled":       false,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		// Its token carries no role of the realm's own: what it may do is the
		// realm-management roles granted below, and a full scope would put
		// every realm role into every token it mints.
		"fullScopeAllowed": false,
	}
	_, err := c.doAdminExpect(ctx, token, http.MethodPost,
		"/admin/realms/"+url.PathEscape(realm)+"/clients", body,
		http.StatusCreated, http.StatusConflict)
	return err
}

// assertRegistrarClientShape re-states the flags that make this a credential.
func (c *KeycloakAdminClient) assertRegistrarClientShape(ctx context.Context, token, realm string, client *keycloakClientRecord) error {
	if !client.PublicClient && client.ServiceAccountsEnabled && client.Enabled && !client.StandardFlowEnabled {
		return nil
	}
	body := map[string]any{
		"clientId":                  RegistrarClientID,
		"enabled":                   true,
		"publicClient":              false,
		"serviceAccountsEnabled":    true,
		"standardFlowEnabled":       false,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"fullScopeAllowed":          false,
	}
	_, err := c.doAdminExpect(ctx, token, http.MethodPut,
		fmt.Sprintf("/admin/realms/%s/clients/%s", url.PathEscape(realm), url.PathEscape(client.ID)),
		body, http.StatusNoContent, http.StatusOK)
	return err
}

// grantRegistrarRoles gives the client's service account the realm-management
// roles in registrarRealmRoles, and only those.
//
// Additive against what is already there, by design: Keycloak's grant endpoint
// adds, and a role somebody granted by hand is not this function's to remove
// silently. What it guarantees is that the five are present.
func (c *KeycloakAdminClient) grantRegistrarRoles(ctx context.Context, token, realm, clientUUID string) error {
	// The client that PROVIDES the roles. Its internal id is generated with
	// the realm, which is why this whole operation is a runtime step and not
	// a composition.
	management, err := c.findClient(ctx, token, realm, "realm-management")
	if err != nil {
		return err
	}
	if management == nil {
		return fmt.Errorf("keycloak realm %s has no realm-management client", realm)
	}

	// The service account user, which Keycloak creates with the client.
	var account struct {
		ID string `json:"id"`
	}
	if err := c.getAdminJSON(ctx, token, fmt.Sprintf(
		"/admin/realms/%s/clients/%s/service-account-user",
		url.PathEscape(realm), url.PathEscape(clientUUID)), &account); err != nil {
		return fmt.Errorf("keycloak realm %s: service account for %s: %w", realm, RegistrarClientID, err)
	}
	if account.ID == "" {
		return fmt.Errorf("keycloak realm %s: %s has no service account", realm, RegistrarClientID)
	}

	var available []keycloakRoleRecord
	if err := c.getAdminJSON(ctx, token, fmt.Sprintf(
		"/admin/realms/%s/users/%s/role-mappings/clients/%s/available",
		url.PathEscape(realm), url.PathEscape(account.ID), url.PathEscape(management.ID)), &available); err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, r := range registrarRealmRoles {
		wanted[r] = true
	}
	var grant []keycloakRoleRecord
	for _, r := range available {
		if wanted[r.Name] {
			grant = append(grant, r)
		}
	}
	// Everything wanted is already held: "available" lists what is NOT yet
	// granted, so an empty intersection means there is nothing to do.
	if len(grant) > 0 {
		if _, err := c.doAdminExpect(ctx, token, http.MethodPost, fmt.Sprintf(
			"/admin/realms/%s/users/%s/role-mappings/clients/%s",
			url.PathEscape(realm), url.PathEscape(account.ID), url.PathEscape(management.ID)),
			grant, http.StatusNoContent, http.StatusOK); err != nil {
			return err
		}
	}
	return c.scopeRegistrarRoles(ctx, token, realm, clientUUID, management.ID)
}

// scopeRegistrarRoles puts the same roles into the client's scope, so its
// tokens carry them.
//
// Holding a role is not enough. The client is fullScopeAllowed: false, on
// purpose, and a client without a full scope mints tokens holding only the
// roles in its scope mappings -- none, until this runs. Keycloak's admin API
// reads roles from the token, so the service account held all five and every
// People call answered 403. Mapped here are exactly the granted five, which
// keeps the narrow scope the flag exists for.
func (c *KeycloakAdminClient) scopeRegistrarRoles(ctx context.Context, token, realm, clientUUID, managementUUID string) error {
	base := fmt.Sprintf("/admin/realms/%s/clients/%s/scope-mappings/clients/%s",
		url.PathEscape(realm), url.PathEscape(clientUUID), url.PathEscape(managementUUID))
	var available []keycloakRoleRecord
	if err := c.getAdminJSON(ctx, token, base+"/available", &available); err != nil {
		return fmt.Errorf("keycloak realm %s: scope of %s: %w", realm, RegistrarClientID, err)
	}
	wanted := map[string]bool{}
	for _, r := range registrarRealmRoles {
		wanted[r] = true
	}
	var scope []keycloakRoleRecord
	for _, r := range available {
		if wanted[r.Name] {
			scope = append(scope, r)
		}
	}
	if len(scope) == 0 {
		return nil
	}
	_, err := c.doAdminExpect(ctx, token, http.MethodPost, base, scope, http.StatusNoContent, http.StatusOK)
	return err
}

// clientSecret reads the client's current secret.
func (c *KeycloakAdminClient) clientSecret(ctx context.Context, token, realm, clientUUID string) (string, error) {
	var out struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := c.getAdminJSON(ctx, token, fmt.Sprintf(
		"/admin/realms/%s/clients/%s/client-secret",
		url.PathEscape(realm), url.PathEscape(clientUUID)), &out); err != nil {
		return "", err
	}
	if out.Value == "" {
		return "", fmt.Errorf("keycloak realm %s: %s has no secret", realm, RegistrarClientID)
	}
	return out.Value, nil
}

// RetiredDirectorClientID is the name this credential had while the director
// held it. The registrar's is a new client rather than the old one renamed,
// so that no secret the director ever mounted opens anything.
const RetiredDirectorClientID = "gentian-director-admin"

// DeleteRetiredDirectorRealmClient removes the director's former credential
// from one realm. Absent is the ordinary answer and not an error.
func (c *KeycloakAdminClient) DeleteRetiredDirectorRealmClient(ctx context.Context, realm string) error {
	return c.deleteRealmClient(ctx, realm, RetiredDirectorClientID)
}

// DeleteRegistrarRealmClient removes the credential for one realm.
//
// The counterpart to Ensure, for a tenant that is retired while its realm
// outlives it: the registrar should stop being able to reach a realm it no
// longer serves, and that is one deletion rather than a rotation.
func (c *KeycloakAdminClient) DeleteRegistrarRealmClient(ctx context.Context, realm string) error {
	return c.deleteRealmClient(ctx, realm, RegistrarClientID)
}

func (c *KeycloakAdminClient) deleteRealmClient(ctx context.Context, realm, clientID string) error {
	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}
	client, err := c.findClient(ctx, token, realm, clientID)
	if err != nil || client == nil {
		return err
	}
	_, err = c.doAdminExpect(ctx, token, http.MethodDelete, fmt.Sprintf(
		"/admin/realms/%s/clients/%s", url.PathEscape(realm), url.PathEscape(client.ID)),
		nil, http.StatusNoContent, http.StatusNotFound)
	return err
}
