/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
	"github.com/gentian-org/gentian-os/internal/registrar/record"
)

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// NewFromEnv wires the server from the registrar's environment.
//
// reader reads the Tenants and the Cluster claim. graph is the authorization
// store: required, because the registrar asks it before every route and one
// with no store to ask would have to refuse everybody or let something else
// decide.
//
// The returned closer releases the record's database connection.
func NewFromEnv(ctx context.Context, reader client.Reader, graph authz.Checker, log *slog.Logger) (*Server, func(), error) {
	if graph == nil {
		return nil, nil, fmt.Errorf("the registrar needs the authorization store (OPENFGA_API_URL): it decides who may manage people")
	}
	cluster := os.Getenv("GENTIAN_DEPLOYMENTS_CLUSTER_ID")
	if cluster == "" {
		return nil, nil, fmt.Errorf("the registrar needs GENTIAN_DEPLOYMENTS_CLUSTER_ID: the cluster is what cluster verbs are asked about")
	}
	issuer := os.Getenv("REGISTRAR_ISSUER_BASE_URL")
	if issuer == "" {
		return nil, nil, fmt.Errorf("the registrar needs REGISTRAR_ISSUER_BASE_URL to verify a caller's token")
	}
	verifier, err := authn.NewVerifier(authn.Config{
		IssuerBase: issuer,
		JWKSBase:   os.Getenv("REGISTRAR_JWKS_BASE_URL"),
		// The zone's token, minted for the director and relayed here by a
		// console as it is relayed to the director.
		Audience: envOr("REGISTRAR_AUDIENCE", "gentian-director"),
	})
	if err != nil {
		return nil, nil, err
	}

	// How the registrar speaks for Keycloak: one credential per realm, handed
	// over by the operator as a mounted Secret with a key per realm. The
	// source re-reads, because on a first install the operator writes the
	// Secret minutes after this process starts and a tenant's realm arrives
	// later still; a realm with no credential yet is a 503 naming it, from
	// the route.
	realmDir := envOr("REGISTRAR_REALM_CREDENTIALS_PATH", "/etc/gentian/realms")
	idClient, err := identity.New(identity.Config{
		// In-cluster. The public issuer refuses /admin by design.
		BaseURL: envOr("REGISTRAR_IDENTITY_BASE_URL", issuer),
		Source:  identity.NewDirectorySource(realmDir, identity.ClientID),
		// The groups whose members hold a platform role, from the Cluster
		// claim on every write. The registrar refuses to change any of them
		// or anybody in one (identity/guard.go).
		PlatformRoleGroups: ClaimPlatformRoleGroups(reader),
		Logger:             log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("identity: %w", err)
	}
	if realms := idClient.Realms(); len(realms) == 0 {
		log.Info("no realm credentials yet; the operator writes one per realm", "path", realmDir)
	} else {
		log.Info("speaking for realms", "realms", realms, "path", realmDir)
	}

	// The durable record of who was allowed to ask for a change to a person.
	// Optional on purpose: a cluster whose database has not been provisioned
	// yet keeps the log line and starts anyway, because refusing to serve at
	// all would take the people screens down over an audit trail. The warning
	// says which it is.
	var authorityRecord *record.Store
	closer := func() {}
	if dsn := os.Getenv("REGISTRAR_DATABASE_URL"); dsn != "" {
		authorityRecord, err = record.Open(ctx, dsn, recordRetention(log))
		if err != nil {
			return nil, nil, fmt.Errorf("registrar database: %w", err)
		}
		closer = authorityRecord.Close
		go pruneRecord(ctx, authorityRecord, log)
	} else {
		log.Warn("REGISTRAR_DATABASE_URL is not set; identity actions are logged but not recorded")
	}

	srv, err := New(Config{
		Authn:             verifier,
		Authz:             graph,
		Tenants:           &ClusterTenants{Client: reader, KernelDomain: os.Getenv("KERNEL_DOMAIN")},
		Identity:          idClient,
		Record:            authorityRecord,
		Log:               log,
		Cluster:           cluster,
		InviteClientID:    os.Getenv("REGISTRAR_INVITE_CLIENT_ID"),
		InviteRedirectURI: os.Getenv("REGISTRAR_INVITE_REDIRECT_URI"),
		// Where a tenant's desktop API answers, %s for the tenant: the
		// settings templates an invitation may apply live there.
		DesktopAPI: os.Getenv("REGISTRAR_DESKTOP_API_URL"),
	})
	if err != nil {
		closer()
		return nil, nil, err
	}
	srv.Addr = envOr("REGISTRAR_ADDR", ":9445")
	return srv, closer, nil
}

// recordRetention is how long an authority record is kept, from
// REGISTRAR_RECORD_RETENTION (a Go duration). Personal data needs a horizon
// somebody chose, and an unparseable value is a misconfiguration worth saying
// out loud rather than silently becoming the default.
func recordRetention(log *slog.Logger) time.Duration {
	raw := os.Getenv("REGISTRAR_RECORD_RETENTION")
	if raw == "" {
		return record.DefaultRetention
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warn("REGISTRAR_RECORD_RETENTION is not a duration; using the default",
			"value", raw, "default", record.DefaultRetention.String())
		return record.DefaultRetention
	}
	return d
}

// pruneRecord removes what is past the horizon, once at start and daily after.
//
// In the process rather than a CronJob: it is one DELETE against a database
// only this process has a credential for, and a CronJob would need its own
// copy of that credential to run it.
func pruneRecord(ctx context.Context, store *record.Store, log *slog.Logger) {
	for {
		pruneCtx, cancel := context.WithTimeout(ctx, time.Minute)
		n, err := store.Prune(pruneCtx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Error("could not prune the authority record", "error", err.Error())
		case n > 0:
			log.Info("pruned authority records past their retention", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}
