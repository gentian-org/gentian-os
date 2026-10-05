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

// usher answers a signed-in person's reads: what is here, and what they may
// open (operator-split-plan.md §3.10).
//
// It runs beside the bouncer from the same image and with the same two
// dependencies -- the identity provider's keys and the authorization store --
// and holds nothing else: no git credential, no signing key, no Kubernetes
// access. What it lists is the operator's projection, mounted as a file.
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

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
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

	srv := &http.Server{
		Addr: envOr("USHER_LISTEN", ":8080"),
		Handler: usher.New(usher.Config{
			Authn: verifier, Authz: store, Log: log,
			TilesPath: envOr("USHER_TILES_PATH", "/etc/gentian/tiles/tiles.yaml"),
		}),
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
