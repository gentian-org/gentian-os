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
// Package entitlement verifies what the App Store states about a tenant's
// right to install a catalogue entry, and makes the cluster agree with it.
//
// The store runs outside the cluster and the cluster never calls it. What
// crosses the boundary is a signed statement — a compact JWS, EdDSA — that
// somebody delivers: "tenant T of cluster C may install entry E until X", or
// "may no longer". The director believes a statement because of who signed it,
// against keys pinned in the cluster's own configuration, never because of who
// carried it or where a key could be fetched from.
//
// A statement is bound to one cluster and one tenant, and ordered by when it
// was issued: the newest recorded fact wins, so a grant replayed after the
// revocation that followed it is refused as stale. The fact is committed to
// gentian-deployments, which is what the authorization store is rebuilt from.
package entitlement

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Claims is the payload of a signed statement.
type Claims struct {
	// Issuer identifies the store; informational, since trust is in the key.
	Issuer string `json:"iss,omitempty"`
	// Audience is "cluster:<id>": a statement for another cluster is not one
	// for this cluster, whoever signed it.
	Audience string `json:"aud"`
	// Subject is "tenant:<name>".
	Subject string `json:"sub"`
	// ID identifies the statement in the store's own records.
	ID string `json:"jti"`
	// IssuedAt orders statements about the same entry.
	IssuedAt int64 `json:"iat"`
	// Expiry ends a grant. Required on a grant, ignored on a revocation.
	Expiry int64 `json:"exp,omitempty"`
	// Coordinate is the catalogue entry, <catalogue>/<app>.
	Coordinate string `json:"coordinate"`
	// Granted is false for a revocation or a denial.
	Granted bool `json:"granted"`
	// Reason is required when Granted is false: silence is not an answer a
	// cluster can log.
	Reason string `json:"reason,omitempty"`
	Seats  int    `json:"seats,omitempty"`
}

var (
	// ErrNotBelieved covers every reason a statement is not the store's.
	ErrNotBelieved = errors.New("statement is not verifiably the store's")
	// ErrNotForHere is a genuine statement about another cluster or tenant.
	ErrNotForHere = errors.New("statement is not for this cluster and tenant")
	// ErrMalformed is a genuine statement that does not say enough.
	ErrMalformed = errors.New("statement is incomplete")
)

// Verifier checks statements against the pinned keys of one cluster.
type Verifier struct {
	keys    map[string]ed25519.PublicKey
	cluster string
	now     func() time.Time
}

// NewVerifier returns a Verifier. keys maps the store's key ids to public keys.
func NewVerifier(keys map[string]ed25519.PublicKey, cluster string) (*Verifier, error) {
	if len(keys) == 0 || cluster == "" {
		return nil, errors.New("entitlement: store keys and the cluster id are required")
	}
	return &Verifier{keys: keys, cluster: cluster, now: time.Now}, nil
}

// skew is how far ahead of this cluster's clock a statement may be dated.
const skew = 2 * time.Minute

// Verify returns the claims of a compact JWS if one of the pinned keys signed
// it and it is addressed to tenant in this cluster.
func (v *Verifier) Verify(compact, tenant string) (*Claims, string, error) {
	jws, err := jose.ParseSignedCompact(compact, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrNotBelieved, err)
	}
	kid := jws.Signatures[0].Protected.KeyID
	key, ok := v.keys[kid]
	if !ok {
		return nil, "", fmt.Errorf("%w: key %q is not pinned on this cluster", ErrNotBelieved, kid)
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrNotBelieved, err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if c.Audience != "cluster:"+v.cluster || c.Subject != "tenant:"+tenant {
		return nil, "", fmt.Errorf("%w: addressed to %s, %s", ErrNotForHere, c.Audience, c.Subject)
	}
	now := v.now()
	switch {
	case c.ID == "" || c.IssuedAt == 0:
		return nil, "", fmt.Errorf("%w: jti and iat are required", ErrMalformed)
	case time.Unix(c.IssuedAt, 0).After(now.Add(skew)):
		return nil, "", fmt.Errorf("%w: issued in the future", ErrMalformed)
	case c.Granted && c.Expiry <= c.IssuedAt:
		return nil, "", fmt.Errorf("%w: a grant must expire after it is issued", ErrMalformed)
	case !c.Granted && c.Reason == "":
		return nil, "", fmt.Errorf("%w: a revocation carries a reason", ErrMalformed)
	}
	if _, err := authz.CatalogueEntry(c.Coordinate); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return &c, kid, nil
}

// Repository records facts.
type Repository interface {
	RecordEntitlement(ctx context.Context, tenant string, fact gitops.Entitlement, meta gitops.Meta) (gitops.Result, error)
}

// Store is the part of OpenFGA entitlements need.
type Store interface {
	Read(ctx context.Context, filter authz.Tuple) ([]authz.Tuple, error)
	Write(ctx context.Context, writes, deletes []authz.Tuple) error
}

// Applier makes git and the authorization store agree with a verified statement.
type Applier struct {
	Repo  Repository
	Store Store
}

// Apply records the fact and updates the tuple. The order is the one that
// fails towards less access: a grant is committed before its tuple is written,
// a revocation removes the tuple before it is committed. Either way a failure
// in between is answered as an error, and delivering the same statement again
// completes it.
func (a *Applier) Apply(ctx context.Context, tenant string, c *Claims, kid string, meta gitops.Meta) (gitops.Result, error) {
	entry, err := authz.CatalogueEntry(c.Coordinate)
	if err != nil {
		return gitops.Result{}, err
	}
	fact := gitops.Entitlement{
		Coordinate: c.Coordinate, Granted: c.Granted, IssuedAt: time.Unix(c.IssuedAt, 0).UTC(),
		Reason: c.Reason, Seats: c.Seats, GrantID: c.ID, KeyID: kid,
	}
	if c.Granted {
		exp := time.Unix(c.Expiry, 0).UTC()
		fact.ExpiresAt = &exp
	}

	if !c.Granted {
		if err := a.setTuple(ctx, tenant, entry, nil); err != nil {
			return gitops.Result{}, err
		}
		return a.Repo.RecordEntitlement(ctx, tenant, fact, meta)
	}
	res, err := a.Repo.RecordEntitlement(ctx, tenant, fact, meta)
	if err != nil {
		return gitops.Result{}, err
	}
	if err := a.setTuple(ctx, tenant, entry, fact.ExpiresAt); err != nil {
		return gitops.Result{}, err
	}
	return res, nil
}

// setTuple makes the entitled tuple for (tenant, entry) expire at until, or not
// exist when until is nil. OpenFGA cannot change a tuple's condition in place,
// so a renewal is a delete and a write.
func (a *Applier) setTuple(ctx context.Context, tenant, entry string, until *time.Time) error {
	key := authz.Tuple{User: authz.Tenant(tenant), Relation: "entitled", Object: entry}
	existing, err := a.Store.Read(ctx, key)
	if err != nil {
		return fmt.Errorf("read entitlement tuple: %w", err)
	}
	var writes []authz.Tuple
	if until != nil {
		key.Condition = &authz.Condition{Name: "grant_valid", Context: map[string]any{"expires_at": until.Format(time.RFC3339)}}
		writes = []authz.Tuple{key}
	}
	if len(existing) == 0 && len(writes) == 0 {
		return nil
	}
	if err := a.Store.Write(ctx, writes, existing); err != nil {
		return fmt.Errorf("write entitlement tuple: %w", err)
	}
	return nil
}
