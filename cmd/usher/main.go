/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// usher answers a signed-in person's reads: what is here, and what they may
// open (operator-split-plan.md §3.10).
//
// It runs beside the bouncer from the same image and with the same two
// dependencies -- the identity provider's keys and the authorization store --
// plus one of its own: a token for the operator's listener that admits reads
// and nothing else. It holds no git credential, no signing key and no
// Kubernetes access. What it lists is the operator's projection, mounted as
// a file, and what it says of live state is the operator's answer.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/usher"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("usher stopped", "error", err.Error())
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func run(log *slog.Logger) error {
	issuerBase := os.Getenv("DIRECTOR_ISSUER_BASE_URL")
	fgaURL := os.Getenv("OPENFGA_API_URL")
	if issuerBase == "" || fgaURL == "" {
		return errors.New("DIRECTOR_ISSUER_BASE_URL and OPENFGA_API_URL are required")
	}

	// The same audience the bouncer requires: the zone's edge token, which
	// the desktop's backend relays here as it does to the director.
	verifier, err := authn.NewVerifier(authn.Config{
		IssuerBase: issuerBase,
		JWKSBase:   os.Getenv("DIRECTOR_JWKS_BASE_URL"),
		Audience:   envOr("USHER_AUDIENCE", "gentian-director"),
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

	cfg := usher.Config{
		Authn: verifier, Authz: store, Log: log,
		TilesPath: envOr("USHER_TILES_PATH", "/etc/gentian/tiles/tiles.yaml"),
		Cluster:   os.Getenv("GENTIAN_DEPLOYMENTS_CLUSTER_ID"),
		// Whether the cluster reports what it runs, as the chart rendered it
		// for the operator: on only with an address to report to. Unset is
		// off, and off withholds the App Store.
		LicenceReporting: os.Getenv("USHER_LICENCE_REPORT_ENABLED") == "true",
	}
	// The operator's listener, for reads of live state. The token is the
	// reader's, handed over by the operator in a mounted file and read on
	// every request: the file appears after the operator's first start and
	// this process must not need a restart to notice. While it is missing or
	// empty no token is presented and the operator refuses, which a caller
	// sees as a gateway error rather than as an empty screen.
	if base := os.Getenv("USHER_LIFECYCLE_URL"); base == "" {
		log.Warn("no app-lifecycle URL: reads of live state are not served here", "setting", "USHER_LIFECYCLE_URL")
	} else {
		tokenFile := envOr("USHER_LIFECYCLE_TOKEN_FILE", "/etc/gentian/lifecycle/token")
		cfg.Lifecycle = lifecycle.NewReader(base, func() string {
			token, err := os.ReadFile(tokenFile)
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(token))
		})
		if cfg.Cluster == "" {
			log.Warn("no cluster id: the cluster's own reads are not served here", "setting", "GENTIAN_DEPLOYMENTS_CLUSTER_ID")
		}
	}

	srv := &http.Server{
		Addr:              envOr("USHER_LISTEN", ":8080"),
		Handler:           usher.New(cfg),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Info("usher listening", "addr", srv.Addr, "issuer", issuerBase, "store", fgaOptions.StoreID)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
