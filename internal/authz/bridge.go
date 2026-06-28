// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package authz

import (
	"context"
	"fmt"
)

const (
	StoreName          = "gentian"
	ShellAppObjectID   = "gentian-ui"
	ShellAppObjectType = "shell_app"
)

// Bridge syncs Keycloak identities into OpenFGA relationship tuples.
type Bridge struct {
	OpenFGA   *OpenFGAClient
	Keycloak  *KeycloakAdminClient
	StoreID   string
	ModelID   string
}

// EnsureBootstrap creates the Gentian store and loads authorization model v0.
func (b *Bridge) EnsureBootstrap(ctx context.Context) error {
	if err := b.OpenFGA.Health(ctx); err != nil {
		return fmt.Errorf("openfga health: %w", err)
	}
	if b.StoreID == "" {
		id, found, err := b.OpenFGA.FindStoreByName(ctx, StoreName)
		if err != nil {
			return err
		}
		if !found {
			id, err = b.OpenFGA.CreateStore(ctx, StoreName)
			if err != nil {
				return err
			}
		}
		b.StoreID = id
	}
	if b.ModelID == "" {
		modelID, err := b.OpenFGA.WriteAuthorizationModel(ctx, b.StoreID)
		if err != nil {
			return fmt.Errorf("write authorization model: %w", err)
		}
		b.ModelID = modelID
	}
	return nil
}

// SyncRealmUsers mirrors enabled Keycloak users into tenant membership and shell launch tuples.
func (b *Bridge) SyncRealmUsers(ctx context.Context, realm string) error {
	if b.StoreID == "" {
		return fmt.Errorf("openfga store not bootstrapped")
	}
	if err := b.Keycloak.EnsureRealm(ctx, realm, realm); err != nil {
		return fmt.Errorf("ensure keycloak realm %s: %w", realm, err)
	}
	users, err := b.Keycloak.ListRealmUsers(ctx, realm)
	if err != nil {
		return err
	}
	tuples := []Tuple{
		{
			User:     ObjectRef("tenant", realm),
			Relation: "parent",
			Object:   ObjectRef(ShellAppObjectType, ShellAppObjectID),
		},
	}
	for _, u := range users {
		subject := UserSubject(u.ID)
		tuples = append(tuples,
			Tuple{User: subject, Relation: "member", Object: ObjectRef("tenant", realm)},
		)
	}
	return b.OpenFGA.WriteTuples(ctx, b.StoreID, tuples, nil)
}
