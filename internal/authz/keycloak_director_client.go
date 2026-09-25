/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package authz

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/identity"
)

// The director's administrative credential in one realm.
//
// The director speaks for Keycloak on a caller's behalf (S7A.17) and holds one
// credential PER REALM rather than one that can reach every realm. That is the
// whole security argument: the director authorises per tenant, so a missed
// check should not be able to reach a realm the caller has nothing to do with,
// and a credential that only exists for one realm makes that structural rather
// than policed.
//
// This is the operator's work, not the director's, and not Crossplane's.
//
//   - Not the director's: it would have to hold a credential that can create
//     administrative clients in order to create the one it is allowed to use,
//     which is the privilege the split exists to avoid.
//   - Not Crossplane's: provider-keycloak's ClientServiceAccountRole needs the
//     INTERNAL id of the realm-management client that provides the role, and
//     that id is generated when the realm is created. Terraform reads it with a
//     data source; the provider has none, and there is nothing to seed an
//     external-name with. Looking it up is a runtime step, so it belongs in a
//     reconciler.

// DirectorClientID is the client the director authenticates as. Taken from
// the director's own package rather than spelled again here: the operator
// writes this credential and the director asks for it by name, and a name the
// two disagree about produces a realm the director cannot speak for with no
// error anywhere to say why.
const DirectorClientID = identity.ClientID

// directorRealmRoles are the realm-management roles the director is granted,
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
// manage-authorization, impersonation, view-events. The director configures no
// clients (compositions do), brokers no identities, and reads events through
// its own read path rather than by holding the role that can also clear them.
var directorRealmRoles = []string{
	"view-users",
	"query-users",
	"query-groups",
	"manage-users",
	"manage-realm",
}

// DirectorRealmRoles is the list, for a caller that wants to report or assert
// it rather than re-spell it.
func DirectorRealmRoles() []string {
	out := append([]string(nil), directorRealmRoles...)
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

// EnsureDirectorRealmClient makes the director's credential for one realm
// exist, and returns its secret.
//
// Idempotent: it is what a reconciler calls on every pass. It does not rotate
// the secret -- a rotation is a deliberate act with a window in which the
// director must be handed the new value before the old one stops working, and
// doing it silently on a reconcile would sign people out of a screen halfway
// through using it.
func (c *KeycloakAdminClient) EnsureDirectorRealmClient(ctx context.Context, realm string) (string, error) {
	if realm == "" || strings.ContainsAny(realm, "/?#") {
		return "", fmt.Errorf("not a realm name: %q", realm)
	}
	token, err := c.adminToken(ctx)
	if err != nil {
		return "", err
	}

	client, err := c.findClient(ctx, token, realm, DirectorClientID)
	if err != nil {
		return "", err
	}
	if client == nil {
		if err := c.createDirectorClient(ctx, token, realm); err != nil {
			return "", err
		}
		client, err = c.findClient(ctx, token, realm, DirectorClientID)
		if err != nil {
			return "", err
		}
		if client == nil {
			return "", fmt.Errorf("keycloak realm %s: created %s and cannot find it", realm, DirectorClientID)
		}
	}

	// The flags are asserted on every pass, not only at creation. A client
	// somebody turned into a public one, or whose service account was
	// disabled, is a credential that silently stops working -- and in the
	// public case, one that stops being a credential at all.
	if err := c.assertDirectorClientShape(ctx, token, realm, client); err != nil {
		return "", err
	}
	if err := c.grantDirectorRoles(ctx, token, realm, client.ID); err != nil {
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

func (c *KeycloakAdminClient) createDirectorClient(ctx context.Context, token, realm string) error {
	body := map[string]any{
		"clientId":    DirectorClientID,
		"name":        "Gentian director (administrative)",
		"description": "The director acts for a caller the platform has already authorised. Its authority is the caller's; this credential is only how it reaches this realm.",
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

// assertDirectorClientShape re-states the flags that make this a credential.
func (c *KeycloakAdminClient) assertDirectorClientShape(ctx context.Context, token, realm string, client *keycloakClientRecord) error {
	if !client.PublicClient && client.ServiceAccountsEnabled && client.Enabled && !client.StandardFlowEnabled {
		return nil
	}
	body := map[string]any{
		"clientId":                  DirectorClientID,
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

// grantDirectorRoles gives the client's service account the realm-management
// roles in directorRealmRoles, and only those.
//
// Additive against what is already there, by design: Keycloak's grant endpoint
// adds, and a role somebody granted by hand is not this function's to remove
// silently. What it guarantees is that the five are present.
func (c *KeycloakAdminClient) grantDirectorRoles(ctx context.Context, token, realm, clientUUID string) error {
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
		return fmt.Errorf("keycloak realm %s: service account for %s: %w", realm, DirectorClientID, err)
	}
	if account.ID == "" {
		return fmt.Errorf("keycloak realm %s: %s has no service account", realm, DirectorClientID)
	}

	var available []keycloakRoleRecord
	if err := c.getAdminJSON(ctx, token, fmt.Sprintf(
		"/admin/realms/%s/users/%s/role-mappings/clients/%s/available",
		url.PathEscape(realm), url.PathEscape(account.ID), url.PathEscape(management.ID)), &available); err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, r := range directorRealmRoles {
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
	if len(grant) == 0 {
		return nil
	}
	_, err = c.doAdminExpect(ctx, token, http.MethodPost, fmt.Sprintf(
		"/admin/realms/%s/users/%s/role-mappings/clients/%s",
		url.PathEscape(realm), url.PathEscape(account.ID), url.PathEscape(management.ID)),
		grant, http.StatusNoContent, http.StatusOK)
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
		return "", fmt.Errorf("keycloak realm %s: %s has no secret", realm, DirectorClientID)
	}
	return out.Value, nil
}

// DeleteDirectorRealmClient removes the credential for one realm.
//
// The counterpart to Ensure, for a tenant that is retired while its realm
// outlives it: the director should stop being able to reach a realm it no
// longer serves, and that is one deletion rather than a rotation.
func (c *KeycloakAdminClient) DeleteDirectorRealmClient(ctx context.Context, realm string) error {
	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}
	client, err := c.findClient(ctx, token, realm, DirectorClientID)
	if err != nil || client == nil {
		return err
	}
	_, err = c.doAdminExpect(ctx, token, http.MethodDelete, fmt.Sprintf(
		"/admin/realms/%s/clients/%s", url.PathEscape(realm), url.PathEscape(client.ID)),
		nil, http.StatusNoContent, http.StatusNotFound)
	return err
}
