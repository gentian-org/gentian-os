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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// KeycloakAdminSecret is the Secret that holds Keycloak's admin endpoint
// (url) and the client credential (client_id, client_secret) the platform
// administers realms with. External Secrets projects it beside Keycloak, in
// the layout's authentication namespace, where the identity Jobs mount it.
const KeycloakAdminSecret = "keycloak-admin"

// LoadKeycloakAdmin reads Keycloak's admin endpoint and client credential
// from KeycloakAdminSecret. Surrounding whitespace is dropped: a value
// stored with a trailing newline would otherwise reach Keycloak as part of
// the credential.
func LoadKeycloakAdmin(ctx context.Context, c client.Reader) (baseURL, clientID, clientSecret string, err error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: KeycloakAdminSecret, Namespace: layout.Namespace(layout.Authentication)}
	if err := c.Get(ctx, key, secret); err != nil {
		return "", "", "", fmt.Errorf("get Secret %s: %w", key, err)
	}
	baseURL = strings.TrimSpace(string(secret.Data["url"]))
	clientID = strings.TrimSpace(string(secret.Data["client_id"]))
	clientSecret = strings.TrimSpace(string(secret.Data["client_secret"]))
	if baseURL == "" || clientID == "" || clientSecret == "" {
		return "", "", "", fmt.Errorf("%s secret missing url, client_id or client_secret", KeycloakAdminSecret)
	}
	return baseURL, clientID, clientSecret, nil
}

// LoadKeycloakAdminClient is a KeycloakAdminClient for the credential in
// KeycloakAdminSecret.
func LoadKeycloakAdminClient(ctx context.Context, c client.Reader) (*KeycloakAdminClient, error) {
	baseURL, clientID, clientSecret, err := LoadKeycloakAdmin(ctx, c)
	if err != nil {
		return nil, err
	}
	return NewKeycloakAdminClient(baseURL, clientID, clientSecret), nil
}
