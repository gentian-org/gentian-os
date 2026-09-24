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
// Command director is the platform's only writer to gentian-deployments.
//
// It authenticates every request against Keycloak, asks OpenFGA whether the
// caller may make the change, and commits it as the caller. It holds the git
// push credential and no cluster credential; the operator holds the reverse.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/entitlement"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/membership"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("director stopped", "error", err.Error())
		os.Exit(1)
	}
}

// required reads settings that have no safe default. They are collected so a
// misconfigured director names everything that is missing at once.
type required struct{ missing []string }

func (r *required) env(name string) string {
	v := os.Getenv(name)
	if v == "" {
		r.missing = append(r.missing, name)
	}
	return v
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func run(log *slog.Logger) error {
	var req required
	issuerBase := req.env("DIRECTOR_ISSUER_BASE_URL")
	audience := req.env("DIRECTOR_AUDIENCE")
	fgaURL := req.env("OPENFGA_API_URL")
	// The store and the model are the director's to create: they exist only
	// once OpenFGA answers, and a cluster rebuilt from git has to arrive at
	// the same place without anyone remembering to run something. Set them to
	// pin a store a cluster already has.
	fgaStore := os.Getenv("OPENFGA_STORE_ID")
	fgaModel := os.Getenv("OPENFGA_MODEL_ID")
	repoPath := req.env("GENTIAN_DEPLOYMENTS_PATH")
	repoURL := req.env("GENTIAN_DEPLOYMENTS_REPO")
	cluster := req.env("GENTIAN_DEPLOYMENTS_CLUSTER_ID")
	if len(req.missing) > 0 {
		return fmt.Errorf("missing configuration: %v", req.missing)
	}

	// Entitlements are enforced unless a deployment says otherwise, in so many
	// words. A cluster with no store has nothing that could grant one, and
	// turns this off as a visible setting.
	enforce := true
	switch v := envOr("DIRECTOR_ENTITLEMENTS", "enforce"); v {
	case "enforce":
	case "off":
		enforce = false
		log.Warn("entitlements are not enforced: any catalogue entry may be installed by whoever may install", "setting", "DIRECTOR_ENTITLEMENTS=off")
	default:
		return fmt.Errorf("DIRECTOR_ENTITLEMENTS must be enforce or off, not %q", v)
	}

	verifier, err := authn.NewVerifier(authn.Config{
		IssuerBase: issuerBase,
		JWKSBase:   os.Getenv("DIRECTOR_JWKS_BASE_URL"),
		Audience:   audience,
	})
	if err != nil {
		return err
	}
	fgaOptions := authz.Options{BaseURL: fgaURL, APIToken: os.Getenv("OPENFGA_API_TOKEN"), Logger: log}
	// Found, never created.
	//
	// The director decides; it does not establish. The store and the model are
	// the operator's to create, because projecting declared state into cluster
	// state is its work and not this service's, and because a service that
	// only has to READ the graph to answer a question should not hold a
	// credential that can rewrite it. Lookup is the same read-only discovery
	// the edge authorization service uses.
	if fgaStore == "" || fgaModel == "" {
		lookupCtx, cancelLookup := context.WithTimeout(context.Background(), 2*time.Minute)
		store, model, err := authz.Lookup(lookupCtx, fgaOptions)
		cancelLookup()
		if err != nil {
			return fmt.Errorf("authorization graph: %w "+
				"(the operator creates the store and the model; this service only reads them)", err)
		}
		if fgaStore == "" {
			fgaStore = store
		}
		if fgaModel == "" {
			fgaModel = model
		}
	}
	fgaOptions.StoreID, fgaOptions.ModelID = fgaStore, fgaModel
	checker, err := authz.NewOpenFGA(fgaOptions)
	if err != nil {
		return err
	}
	repo := gitops.NewGitOps(repoPath, repoURL, cluster, gitops.Person{
		Name:  envOr("DIRECTOR_COMMITTER_NAME", "gentian-director"),
		Email: os.Getenv("DIRECTOR_COMMITTER_EMAIL"),
	})

	// No cluster-role or tenant projection here.
	//
	// Both used to run at this point, from the claim and the tenant manifests
	// in git, and both wrote tuples. They are the operator's now
	// (internal/controller/authz_projection_reconciler.go): the same state,
	// projected by the thing that turns git into cluster state, and reconciled
	// continuously rather than once at whatever moment this process happened
	// to start.
	// Membership is not accepted here any more.
	//
	// Keycloak's listener now states a user's groups to the OPERATOR
	// (internal/controller/membership_listener.go), which projects them like
	// it projects the rest of the graph's structure. It was the last thing
	// this process wrote to the authorization graph, and while it lived here
	// the director's token had to be able to write -- so the rule that the
	// director reads the graph and writes git was enforced by care rather
	// than by the credential. Now it is enforced by the credential.
	// The store is believed only through keys pinned here, from the Cluster
	// claim. No key, no store: statements are refused because the endpoint does
	// not exist, not because a lookup failed.
	var store *api.StoreConfig
	if raw := os.Getenv("DIRECTOR_STORE_KEYS"); raw == "" {
		log.Warn("no store signing key pinned: entitlement statements are not accepted", "setting", "DIRECTOR_STORE_KEYS")
	} else {
		keys, err := membership.ParseKeys(raw)
		if err != nil {
			return fmt.Errorf("DIRECTOR_STORE_KEYS: %w", err)
		}
		sv, err := entitlement.NewVerifier(keys, cluster)
		if err != nil {
			return err
		}
		store = &api.StoreConfig{Verifier: sv, Applier: &entitlement.Applier{Repo: repo, Store: checker}}
	}
	// No back-channel logout endpoint, and nothing to sweep.
	//
	// Ending a session at Keycloak is what ends it. The edge holds a
	// short-lived access token and refreshes it against Keycloak; a refresh
	// against an ended session fails, so access stops within the access
	// token's lifetime, which the realm sets. The director recorded a
	// revocation tuple to make that immediate, which meant it held write
	// access to the authorization store for one event it is not otherwise
	// part of. Deleting it is the cheaper answer.
	handler, err := api.New(api.Config{Authn: verifier, Authz: checker, Repo: repo, Log: log,
		EnforceEntitlements: enforce, Store: store, Cluster: cluster})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              envOr("DIRECTOR_LISTEN", ":8080"),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A write waits on git, which has its own five-minute bound.
		WriteTimeout: 6 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Info("director listening", "addr", srv.Addr, "cluster", cluster, "issuer", issuerBase, "model", fgaModel)

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
