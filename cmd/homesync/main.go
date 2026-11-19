/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Command homesync keeps the calendars and contacts the tenant's apps keep of
// their own equal to the tenant's home (internal/homesync). One runs per
// tenant.
//
// Settings, all from the environment:
//
//	TENANT_ID           the tenant
//	PROFILE             the service's own profile, as the realm knows it
//	PLAN_FILE           what to sync, as the platform writes it
//	DATABASE_URL        where the pairs are kept
//	VOUCHING_ISSUER, VOUCHING_CLIENT_ID, VOUCHING_CLIENT_SECRET, VOUCHING_TOKEN_URL
//	                    the client the platform gave it (vouching-<profile>)
//	REGISTRAR_URL, REGISTRAR_KEY
//	                    where it asks who is linked to it, and its key there
//	LISTEN              where it publishes its keys (default :8080)
//	INTERVAL            the pause between rounds (default 5m)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gentian-org/gentian-os/internal/homesync"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("homesync stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	need := func(name string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", errors.New(name + " is not set")
		}
		return v, nil
	}
	var missing error
	get := func(name string) string {
		v, err := need(name)
		if err != nil && missing == nil {
			missing = err
		}
		return v
	}
	tenant, profile, plan, dsn := get("TENANT_ID"), get("PROFILE"), get("PLAN_FILE"), get("DATABASE_URL")
	vouching := homesync.Vouching{
		Issuer: get("VOUCHING_ISSUER"), ClientID: get("VOUCHING_CLIENT_ID"),
		ClientSecret: get("VOUCHING_CLIENT_SECRET"), TokenURL: get("VOUCHING_TOKEN_URL"),
	}
	registrar, key := get("REGISTRAR_URL"), get("REGISTRAR_KEY")
	if missing != nil {
		return missing
	}
	interval := 5 * time.Minute
	if v := os.Getenv("INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 30*time.Second {
			return errors.New("INTERVAL is a duration of at least 30s")
		}
		interval = d
	}
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = ":8080"
	}

	keys, err := homesync.NewKeys()
	if err != nil {
		return err
	}
	jwks, err := keys.JWKS()
	if err != nil {
		return err
	}
	store, err := homesync.OpenPostgres(ctx, dsn)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	engine := &homesync.Engine{
		Plan:    func() (*homesync.Plan, error) { return homesync.ReadPlan(plan) },
		People:  (&homesync.Linked{Registrar: registrar, Key: key, Tenant: tenant, Profile: profile, Client: client}).People,
		Tokens:  &homesync.Tokens{Keys: keys, Vouching: vouching, Client: client},
		Store:   store,
		Drivers: homesync.Drivers(client),
		Client:  client,
		Log:     log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the keys are not published", "error", err.Error())
			stop()
		}
	}()
	log.Info("homesync started", "tenant", tenant, "interval", interval.String())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := engine.Round(ctx); err != nil {
			log.Warn("a round failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdown)
		case <-ticker.C:
		}
	}
}
