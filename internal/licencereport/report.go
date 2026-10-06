/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package licencereport tells the receiving endpoint, in the open, what this
// cluster runs.
//
// The software is free under a usage limit that is enforced legally and not
// technically. So nothing here gates anything: the answer to a report is read
// for its status and for nothing else, and a cluster whose reports fail runs
// exactly as one whose reports arrive.
//
// What is sent is counts and addresses of things, never of people: the
// cluster's id and address, each tenant's address and how many accounts its
// realm holds, and for each app installed through the App Store its
// coordinate, the digest it is pinned to, and how many people are entitled to
// it. No name, e-mail address, user id, group name, tenant display name or
// administrator's address is in it, and the types below have nowhere to put
// one.
//
// Every report is signed with a key this cluster alone holds and is recorded,
// exactly as sent, where an operations screen can read it. A cluster with
// reporting turned off, or with no address to report to, sends nothing, ever.
package licencereport

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	// Version is the body's format. A receiver that meets a version it does
	// not know should refuse the report rather than guess at it.
	Version = 1

	// SignatureHeader carries "ed25519=<base64>": an Ed25519 signature over
	// the request body, byte for byte.
	SignatureHeader = "X-Gentian-Signature"
	// KeyIDHeader names the key that signed: the first sixteen hex characters
	// of the SHA-256 of the public key, which is itself in the body.
	KeyIDHeader = "X-Gentian-Key-Id"

	signaturePrefix = "ed25519="
)

// Settings is whether this cluster reports, and to where. It is fixed at
// install time and read from the environment.
type Settings struct {
	Enabled bool
	URL     string
}

// SettingsFromEnv reads the two values the chart renders. Unset means off.
func SettingsFromEnv() Settings {
	return Settings{
		Enabled: os.Getenv("LICENCE_REPORT_ENABLED") == "true",
		URL:     strings.TrimSpace(os.Getenv("LICENCE_REPORT_URL")),
	}
}

// Active reports whether anything is sent at all: reporting is on and there
// is an https address to send to.
//
// An address that is not https counts as none. The body is not secret, but it
// is this cluster's statement about itself, and it is not put on a wire where
// anybody on the path can read which tenants a cluster has.
func (s Settings) Active() bool {
	return s.Enabled && strings.HasPrefix(s.URL, "https://")
}

// Report is the body, whole. Its key set is the contract: a field added here
// is a field sent to somebody else.
type Report struct {
	Version int `json:"version"`
	// Sequence counts this cluster's reports. It never repeats, so a receiver
	// can tell a replayed report from a new one.
	Sequence int64 `json:"sequence"`
	// SentAt is when the report was made, RFC 3339, UTC.
	SentAt  string   `json:"sentAt"`
	Cluster Cluster  `json:"cluster"`
	Tenants []Tenant `json:"tenants"`
	// PublicKey is the base64 Ed25519 key the signature verifies with.
	PublicKey string `json:"publicKey"`
}

// Cluster is which cluster is speaking.
type Cluster struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Tenant is one tenant, by its address.
type Tenant struct {
	URL string `json:"url"`
	// Users is how many accounts the tenant's realm holds. Null when they
	// could not be counted, which is not the same as none.
	Users *int  `json:"users"`
	Apps  []App `json:"apps"`
}

// App is one app installed through the App Store.
type App struct {
	// Coordinate is <catalogue>/<app>. Null when the install did not record
	// which catalogue its build was fetched from; the digest still says
	// which build it is.
	Coordinate *string `json:"coordinate"`
	Digest     string  `json:"digest"`
	// Users is how many people are entitled to the app. Null when they could
	// not be counted.
	Users *int `json:"users"`
}

// Counter says how many people there are. It answers numbers and nothing
// about who.
type Counter interface {
	// TenantUsers is the number of accounts in the tenant's realm.
	TenantUsers(ctx context.Context, tenant *gentianov1alpha1.Tenant) (int, error)
	// AppUsers is the number of people entitled to one of the tenant's apps.
	AppUsers(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile string) (int, error)
}

// Identity is what the cluster says of itself in a report.
type Identity struct {
	// ClusterID is the id this cluster is known by.
	ClusterID string
	// KernelDomain is the cluster's own domain; its address is https://<it>.
	KernelDomain string
	// TenancyMode decides how a tenant's host is derived from the domain.
	TenancyMode string
}

// Build assembles the tenants' part of a report from what the cluster holds.
//
// Only an entry of spec.apps that carries a digest is listed. The director
// records a digest on an entry when an install states the build it wants,
// which is what the App Store's confirmation and a catalogue source's listing
// do; an app that arrived with the kernel, or was named with no build, has
// none and is not reported.
//
// A count that cannot be had is sent as null and the report still goes: a
// number that is missing is said to be missing, and is never replaced by a
// smaller one.
func Build(ctx context.Context, c client.Reader, counter Counter, id Identity, uncounted func(what string, err error)) ([]Tenant, error) {
	var list gentianov1alpha1.TenantList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	if uncounted == nil {
		uncounted = func(string, error) {}
	}
	out := []Tenant{}
	for i := range list.Items {
		tenant := &list.Items[i]
		if !tenant.DeletionTimestamp.IsZero() {
			continue
		}
		host := tenant.EffectiveDomain(id.KernelDomain, id.TenancyMode)
		if host == "" {
			// A tenant with no host has no address to be reported under, and
			// its name is not sent in the address's place.
			continue
		}
		entry := Tenant{URL: "https://" + host, Apps: []App{}}
		if n, err := counter.TenantUsers(ctx, tenant); err != nil {
			uncounted("the accounts of a tenant", err)
		} else {
			entry.Users = &n
		}
		for _, app := range tenant.Spec.Apps {
			if app.Digest == "" {
				continue
			}
			profile := app.Profile
			if profile == "" && app.ProfileRef != nil {
				profile = app.ProfileRef.Name
			}
			listed := App{Digest: app.Digest}
			if app.Catalogue != "" && profile != "" {
				coordinate := app.Catalogue + "/" + profile
				listed.Coordinate = &coordinate
			}
			if profile != "" {
				if n, err := counter.AppUsers(ctx, tenant, profile); err != nil {
					uncounted("the people entitled to an app", err)
				} else {
					listed.Users = &n
				}
			}
			entry.Apps = append(entry.Apps, listed)
		}
		sort.SliceStable(entry.Apps, func(a, b int) bool { return entry.Apps[a].Digest < entry.Apps[b].Digest })
		out = append(out, entry)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].URL < out[b].URL })
	return out, nil
}

// ErrNoKey is a cluster that has no signing key to report with.
var ErrNoKey = errors.New("the signing key is absent")

// Key is this cluster's signing key pair.
type Key struct {
	private ed25519.PrivateKey
}

// KeyFromSeed derives the pair from the 32-byte private seed, given as the 64
// hex characters the installer stores. The seed is never logged and never
// sent, and the errors here do not quote it.
func KeyFromSeed(seed string) (*Key, error) {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return nil, ErrNoKey
	}
	raw, err := hex.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, fmt.Errorf("the signing key is not %d bytes of hex", ed25519.SeedSize)
	}
	return &Key{private: ed25519.NewKeyFromSeed(raw)}, nil
}

// KeyFromFile reads the seed from where the Secret is mounted. A file that is
// not there is a key that is absent, which is a state and not a fault: the
// Secret arrives after the vault path it is read from.
func KeyFromFile(path string) (*Key, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, errors.New("the signing key cannot be read")
	}
	return KeyFromSeed(string(data))
}

// Public is the public half, base64, as the body carries it.
func (k *Key) Public() string {
	return base64.StdEncoding.EncodeToString(k.private.Public().(ed25519.PublicKey))
}

// ID names the key: the first sixteen hex characters of the SHA-256 of its
// public half.
func (k *Key) ID() string {
	sum := sha256.Sum256(k.private.Public().(ed25519.PublicKey))
	return hex.EncodeToString(sum[:])[:16]
}

// Sign is the signature header's value for exactly these bytes.
func (k *Key) Sign(body []byte) string {
	return signaturePrefix + base64.StdEncoding.EncodeToString(ed25519.Sign(k.private, body))
}

// Verify reports whether signature, a SignatureHeader value, is a signature
// over body by the base64 public key. It is what a receiver does, and what
// the tests hold a sent report to.
func Verify(publicKey, signature string, body []byte) bool {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return false
	}
	enc, ok := strings.CutPrefix(signature, signaturePrefix)
	if !ok {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(key), body, sig)
}

// encode renders a report as the bytes that are signed, sent and recorded.
func encode(sequence int64, at time.Time, id Identity, tenants []Tenant, key *Key) ([]byte, error) {
	return json.Marshal(Report{
		Version:   Version,
		Sequence:  sequence,
		SentAt:    at.UTC().Format(time.RFC3339),
		Cluster:   Cluster{ID: id.ClusterID, URL: "https://" + id.KernelDomain},
		Tenants:   tenants,
		PublicKey: key.Public(),
	})
}
