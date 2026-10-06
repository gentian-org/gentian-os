/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/
// Command director is the platform's only writer to gentian-deployments.
//
// It authenticates every request against Keycloak, asks OpenFGA whether the
// caller may make the change, and commits it as the caller. It holds the git
// push credential and no cluster credential; the operator holds the reverse.
// It holds no Keycloak credential either: people are the registrar's
// (cmd/registrar).
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
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
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

	// AD-2: what the director writes is signed, and Argo CD syncs the
	// deployments repository only for commits carrying one of the keys the
	// cluster was told to trust.
	//
	// Warned rather than fatal when the key is absent. External Secrets may
	// project it after this pod started, so it is read again before each
	// commit; until then a repository that names signing keys gets no commit
	// at all, because one unsigned head stops Argo CD syncing all of it. What
	// the director must not do is sign with anything other than the key it
	// was given -- there is no fallback and none is generated.
	if keyPath := os.Getenv("DIRECTOR_SIGNING_KEY_FILE"); keyPath != "" {
		home := envOr("DIRECTOR_GNUPGHOME", "/tmp/gentian-director-gnupg")
		signCtx, cancelSign := context.WithTimeout(context.Background(), time.Minute)
		err := repo.SignFrom(signCtx, keyPath, home)
		cancelSign()
		if err != nil {
			log.Warn("no signing key yet; read again before each commit",
				"path", keyPath, "error", err)
		} else {
			log.Info("commits are signed", "key", repo.SigningKey())
		}
	} else {
		log.Warn("no signing key: commits are unsigned (AD-2)",
			"setting", "DIRECTOR_SIGNING_KEY_FILE")
	}

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
	// No back-channel logout endpoint, and nothing to sweep.
	//
	// Ending a session at Keycloak is what ends it. The edge holds a
	// short-lived access token and refreshes it against Keycloak; a refresh
	// against an ended session fails, so access stops within the access
	// token's lifetime, which the realm sets. The director recorded a
	// revocation tuple to make that immediate, which meant it held write
	// access to the authorization store for one event it is not otherwise
	// part of. Deleting it is the cheaper answer.
	// The tile catalogue is read from a file, not from the Kubernetes API.
	//
	// It is the operator's ConfigMap, mounted into this pod. That keeps the
	// division this process is built on: the operator holds the cluster
	// credential and the director holds none, so the director learns what the
	// cluster routes by being handed it rather than by being trusted to go and
	// look. A cluster whose operator has not projected yet has no file here,
	// and the endpoint answers an empty list.
	// The operator's app-lifecycle API is the one thing in the cluster this
	// process asks a question of, and it only ever asks: a tenant's enforced
	// ceiling, what is under it, which plans it may move to. Choosing a plan
	// is then a commit here, like every other change. Without the URL the
	// resources routes do not exist, which a console shows as exactly that.
	var lc api.Lifecycle
	if u := os.Getenv("DIRECTOR_APP_LIFECYCLE_URL"); u == "" {
		log.Warn("no app-lifecycle URL: a tenant's resources cannot be read or its plan chosen here", "setting", "DIRECTOR_APP_LIFECYCLE_URL")
	} else {
		// The operator's API refuses a request that presents no token, so a
		// director started without one reaches nothing. Saying so here is
		// better than every resources call answering 401 with no clue why.
		token := os.Getenv("APP_LIFECYCLE_TOKEN")
		if token == "" {
			log.Warn("no app-lifecycle token: the operator's API will refuse every request from here",
				"setting", "APP_LIFECYCLE_TOKEN")
		}
		lc = lifecycle.New(u, token)
	}
	// No Keycloak credential, and no record of identity actions.
	//
	// This process spoke for Keycloak once: it held a client credential for
	// every realm and served the people, group and realm-settings routes
	// beside the ones that commit to git. One process could then change both
	// what the cluster runs and who may sign in to it. Those routes, the
	// credential and the database that records who was allowed to ask are
	// the registrar's now (cmd/registrar), and what is left here verifies a
	// caller's token against the issuer's public keys and nothing more.

	// Catalogue sources: where a profile is fetched from when a tenant
	// installs it (AD-3), read from the Cluster claim in git (AD-14).
	//
	// On the claim and not in this process's environment, because opening a
	// catalogue to a tenant is a decision about what software may enter the
	// cluster. On the claim it is a commit with an author and a date, next to
	// everything else the cluster is; in an environment variable it is a
	// value that changed when somebody rolled a Deployment, and the only
	// record is whatever the pod spec says now.
	//
	// The list is read once, at start. A source added to the claim reaches
	// the director when its Deployment next starts -- which is what an Argo
	// sync of a changed claim produces anyway -- and the tuples that say
	// WHICH tenant a source is open to are the operator's, reconciled
	// continuously. Without any source, this cluster's profiles arrive some
	// other way and nothing is materialised on reference.
	var entries *catalogue.Fetcher
	var declared []gitops.CatalogueSource
	var storeURL string
	readSources, cancelRead := context.WithTimeout(context.Background(), time.Minute)
	catalogues, err := repo.Catalogue(readSources)
	cancelRead()
	declared, storeURL = catalogues.Sources, catalogues.StoreURL
	if err != nil {
		// Not fatal. A director that cannot read the claim still serves
		// every call that is not an install from a catalogue, and refusing
		// to start would take the console down with it.
		log.Warn("catalogue sources could not be read from the Cluster claim; nothing will be materialised",
			"error", err)
	} else if len(declared) > 0 {
		sources := make(map[string]string, len(declared))
		for _, src := range declared {
			sources[src.Name] = src.URL
			log.Info("catalogue source", "catalogue", src.Name, "url", src.URL, "openTo", len(src.Tenants))
		}
		entries = catalogue.NewFetcher(sources)
	}
	if storeURL == "" {
		// Worth one line: without it the cluster's own catalogue view can
		// list its ce and pe entries but has nowhere to send anybody for the
		// rest, which looks like the platform having no store at all.
		log.Info("the Cluster claim names no App Store; nothing points anywhere for maintained or licensed entries",
			"setting", "spec.catalogue.storeUrl")
	}

	// The same OpenFGA client answers both questions, and the two are
	// separate interfaces on purpose: Check is the hot path, ViewOf is a
	// person reviewing who holds what. Neither can write through the API.
	handler, err := api.New(api.Config{Authn: verifier, Authz: checker, Viewer: checker, Repo: repo, Log: log,
		Catalogue:        entries,
		CatalogueSources: declared,
		StoreURL:         storeURL,
		Cluster:          cluster,
		Lifecycle:        lc})
	if err != nil {
		return err
	}

	// A purge a previous run asked for and did not see through.
	handler.ResumePurges(context.Background())

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
