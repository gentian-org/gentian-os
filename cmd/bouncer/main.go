/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// bouncer is the ext-auth bouncer: L2 of the edge, the one enforcement point
// between the Gateway's session and every backend (networking.md §2).
//
// It holds one credential -- the store's, if the store wants one -- and no
// state of its own: the route table is the operator's, decisions are the
// store's, revocation arrives through the store's changelog. Several
// replicas are therefore the same replica.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/gentian-org/gentian-os/internal/bouncer"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("bouncer stopped", "error", err.Error())
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envDuration(name, def string) (time.Duration, error) {
	d, err := time.ParseDuration(envOr(name, def))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

func run(log *slog.Logger) error {
	issuerBase := os.Getenv("DIRECTOR_ISSUER_BASE_URL")
	audience := envOr("BOUNCER_AUDIENCE", "gentian-director")
	fgaURL := os.Getenv("OPENFGA_API_URL")
	tablePath := envOr("BOUNCER_ROUTES", "/etc/bouncer/routes.yaml")
	if issuerBase == "" || fgaURL == "" {
		return errors.New("DIRECTOR_ISSUER_BASE_URL and OPENFGA_API_URL are required")
	}
	cacheTTL, err := envDuration("BOUNCER_CACHE_TTL", "5m")
	if err != nil {
		return err
	}
	poll, err := envDuration("BOUNCER_POLL_INTERVAL", "2s")
	if err != nil {
		return err
	}

	verifier, err := authn.NewVerifier(authn.Config{
		IssuerBase: issuerBase,
		JWKSBase:   os.Getenv("DIRECTOR_JWKS_BASE_URL"),
		Audience:   audience,
	})
	if err != nil {
		return err
	}

	// The store the director established; this process never writes one.
	fgaOptions := authz.Options{BaseURL: fgaURL, APIToken: os.Getenv("OPENFGA_API_TOKEN"), Logger: log}
	fgaOptions.StoreID, fgaOptions.ModelID = os.Getenv("OPENFGA_STORE_ID"), os.Getenv("OPENFGA_MODEL_ID")
	if fgaOptions.StoreID == "" || fgaOptions.ModelID == "" {
		lookupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		store, model, err := authz.Lookup(lookupCtx, fgaOptions)
		cancel()
		if err != nil {
			return fmt.Errorf("authorization store: %w", err)
		}
		if fgaOptions.StoreID == "" {
			fgaOptions.StoreID = store
		}
		if fgaOptions.ModelID == "" {
			fgaOptions.ModelID = model
		}
	}
	store, err := authz.NewOpenFGA(fgaOptions)
	if err != nil {
		return err
	}

	table, err := bouncer.LoadTable(tablePath)
	if err != nil {
		// No table is a table with no routes: every host is refused until
		// the operator writes one, which is the safe direction.
		log.Warn("no route table yet; every host is refused until the operator writes one", "path", tablePath, "error", err.Error())
		table = &bouncer.Table{}
	}
	// The exchange of a session's token for an app's own, for the routes
	// that ask for it. The realms are reached where their keys are fetched,
	// and each realm's exchange client presents the secret the operator
	// mounted for it; a realm with no secret there has no exchange, and a
	// route of it that asks for one is refused.
	exchanger := &bouncer.RealmExchanger{
		TokenBase:  envOr("BOUNCER_TOKEN_BASE_URL", envOr("DIRECTOR_JWKS_BASE_URL", issuerBase)),
		SecretsDir: envOr("BOUNCER_EXCHANGE_SECRETS", "/etc/bouncer-exchange"),
	}
	decider := bouncer.New(bouncer.Options{Verifier: verifier, Store: store, Table: table, CacheTTL: cacheTTL, Logger: log, Exchanger: exchanger})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The table follows the ConfigMap: the kubelet swaps the mounted file
	// when the operator changes it, and the modification time says so.
	go watchTable(ctx, tablePath, decider, log)
	// Eviction, not expiry, is what makes a change visible.
	go bouncer.Poll(ctx, store, poll, log, func(n int) {
		if evicted := decider.Evict(); evicted > 0 {
			log.Info("changelog moved; cached decisions evicted", "changes", n, "evicted", evicted)
		}
	})

	lis, err := net.Listen("tcp", envOr("BOUNCER_LISTEN", ":9001"))
	if err != nil {
		return err
	}
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, &bouncer.Server{Decider: decider, BrandingBase: bouncer.BrandingBase(issuerBase)})
	healthpb.RegisterHealthServer(srv, health.NewServer())

	httpSrv := &http.Server{Addr: envOr("BOUNCER_HEALTH", ":8081"), ReadHeaderTimeout: 5 * time.Second}
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	go func() { _ = httpSrv.ListenAndServe() }()

	// The one question a component may ask for a person who is not at a
	// browser, on a port of its own so that the network can admit components
	// to it and to nothing else here.
	checkSrv := &http.Server{Addr: envOr("BOUNCER_CHECK_LISTEN", ":8082"), Handler: decider.CheckHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = checkSrv.ListenAndServe() }()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	log.Info("bouncer listening", "addr", lis.Addr().String(), "routes", len(table.Routes), "issuer", issuerBase, "store", fgaOptions.StoreID)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	srv.GracefulStop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	_ = checkSrv.Shutdown(shutdown)
	return nil
}

func watchTable(ctx context.Context, path string, decider *bouncer.Decider, log *slog.Logger) {
	var last time.Time
	if st, err := os.Stat(path); err == nil {
		last = st.ModTime()
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		if st.ModTime().Equal(last) {
			continue
		}
		last = st.ModTime()
		table, err := bouncer.LoadTable(path)
		if err != nil {
			log.Warn("route table not reloaded", "error", err.Error())
			continue
		}
		decider.SetTable(table)
		decider.Evict()
		log.Info("route table reloaded", "routes", len(table.Routes))
	}
}
