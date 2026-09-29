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

// edge-authz is the ext-auth shim: L2 of the edge, the one enforcement point
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

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	edge "github.com/gentian-org/gentian-os/internal/edge/authz"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("edge-authz stopped", "error", err.Error())
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
	audience := envOr("EDGE_AUTHZ_AUDIENCE", "gentian-director")
	fgaURL := os.Getenv("OPENFGA_API_URL")
	tablePath := envOr("EDGE_AUTHZ_ROUTES", "/etc/edge-authz/routes.yaml")
	if issuerBase == "" || fgaURL == "" {
		return errors.New("DIRECTOR_ISSUER_BASE_URL and OPENFGA_API_URL are required")
	}
	cacheTTL, err := envDuration("EDGE_AUTHZ_CACHE_TTL", "5m")
	if err != nil {
		return err
	}
	poll, err := envDuration("EDGE_AUTHZ_POLL_INTERVAL", "2s")
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

	table, err := edge.LoadTable(tablePath)
	if err != nil {
		// No table is a table with no routes: every host is refused until
		// the operator writes one, which is the safe direction.
		log.Warn("no route table yet; every host is refused until the operator writes one", "path", tablePath, "error", err.Error())
		table = &edge.Table{}
	}
	decider := edge.New(edge.Options{Verifier: verifier, Store: store, Table: table, CacheTTL: cacheTTL, Logger: log})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The table follows the ConfigMap: the kubelet swaps the mounted file
	// when the operator changes it, and the modification time says so.
	go watchTable(ctx, tablePath, decider, log)
	// Eviction, not expiry, is what makes a change visible.
	go edge.Poll(ctx, store, poll, log, func(n int) {
		if evicted := decider.Evict(); evicted > 0 {
			log.Info("changelog moved; cached decisions evicted", "changes", n, "evicted", evicted)
		}
	})

	lis, err := net.Listen("tcp", envOr("EDGE_AUTHZ_LISTEN", ":9001"))
	if err != nil {
		return err
	}
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, &edge.Server{Decider: decider})
	healthpb.RegisterHealthServer(srv, health.NewServer())

	httpSrv := &http.Server{Addr: envOr("EDGE_AUTHZ_HEALTH", ":8081"), ReadHeaderTimeout: 5 * time.Second}
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	go func() { _ = httpSrv.ListenAndServe() }()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	log.Info("edge-authz listening", "addr", lis.Addr().String(), "routes", len(table.Routes), "issuer", issuerBase, "store", fgaOptions.StoreID)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	srv.GracefulStop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	return nil
}

func watchTable(ctx context.Context, path string, decider *edge.Decider, log *slog.Logger) {
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
		table, err := edge.LoadTable(path)
		if err != nil {
			log.Warn("route table not reloaded", "error", err.Error())
			continue
		}
		decider.SetTable(table)
		decider.Evict()
		log.Info("route table reloaded", "routes", len(table.Routes))
	}
}
