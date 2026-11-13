/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// The chaperone carries tool calls to a tenant's adapters.
//
// It runs in a tenant, one per tenant, as a component the platform places.
// It verifies who asks -- a present person's token from the tenant's realm,
// or a pass from the issuer the install names -- asks the platform whether
// that person may use the app, obtains the person's token for the app from
// the realm, hands the call to the app's adapter and writes down what
// happened. It can change nothing on the platform: it holds no credential of
// an app, of the rights store or of the cluster.
//
// Each of its questions is bound by settings. While one is unbound the
// routes that need it answer 503; /readyz says what is bound.
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

	"github.com/gentian-org/gentian-os/internal/chaperone"
	"github.com/gentian-org/gentian-os/internal/envvar"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("chaperone stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, closeJournal, err := bind(ctx, os.Getenv, log)
	if err != nil {
		return err
	}
	defer closeJournal()

	api := &http.Server{
		Addr: envvar.Or("CHAPERONE_LISTEN", ":8000"), Handler: srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- api.ListenAndServe() }()
	log.Info("chaperone listening", "addr", api.Addr)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return api.Shutdown(shutdown)
}

// bind makes the server from the settings. A setting that is absent leaves
// its question unanswered; one that is present and wrong is an error.
func bind(ctx context.Context, env func(string) string, log *slog.Logger) (*chaperone.Server, func(), error) {
	srv := &chaperone.Server{Log: log}
	closeJournal := func() {}
	tenant := env("CHAPERONE_TENANT")

	installed, err := chaperone.ParseInstalled(env("CHAPERONE_ADAPTERS"))
	if err != nil {
		return nil, nil, err
	}
	rightsURL, rightsKey, dsn := env("RIGHTS_CHECK_URL"), env("RIGHTS_CHECK_KEY"), env("DATABASE_URL")
	if tenant != "" && len(installed) > 0 && rightsURL != "" && rightsKey != "" && dsn != "" {
		journal, err := chaperone.OpenJournal(ctx, dsn)
		if err != nil {
			return nil, nil, err
		}
		closeJournal = journal.Close
		adapters := chaperone.NewMcpAdapters(installed, nil)
		srv.Service = &chaperone.Service{
			Rights: chaperone.NewPlatformRights(rightsURL, rightsKey, nil),
			Tools:  adapters, Adapters: adapters, Journal: journal, Tenant: tenant,
		}
	} else {
		log.Warn("every call answers 503: the tenant, the adapters, the rights check or the database is not set",
			"settings", "CHAPERONE_TENANT, CHAPERONE_ADAPTERS, RIGHTS_CHECK_URL, RIGHTS_CHECK_KEY, DATABASE_URL")
	}

	// The tenant's realm, for a person who is present. The issuer is the
	// realm's as its tokens name it; the other three are the platform's
	// Secret vouching-<profile>. The client's id is also the audience a
	// present person's token must name.
	issuer, id, secret, tokenURL := env("OIDC_ISSUER"), env("VOUCHING_CLIENT_ID"), env("VOUCHING_CLIENT_SECRET"), env("VOUCHING_TOKEN_URL")
	if issuer != "" && id != "" && secret != "" && tokenURL != "" {
		srv.Persons = chaperone.NewRealmPersons(issuer, id, chaperone.CertsURL(tokenURL), nil)
		if srv.Service != nil {
			srv.Service.Credentials = chaperone.NewRealmCredentials(tokenURL, id, secret, nil)
		}
	} else {
		log.Warn("a present person's calls answer 503: the realm is not set",
			"settings", "OIDC_ISSUER, VOUCHING_CLIENT_ID, VOUCHING_CLIENT_SECRET, VOUCHING_TOKEN_URL")
	}

	// Who signs passes is the install's to say: the chaperone names nobody.
	keysURL, passIssuer := env("CHAPERONE_PASS_KEYS_URL"), env("CHAPERONE_PASS_ISSUER")
	if keysURL != "" && passIssuer != "" && tenant != "" {
		audience := env("CHAPERONE_PASS_AUDIENCE")
		if audience == "" {
			audience = "chaperone"
		}
		srv.Passes = chaperone.NewIssuerPasses(keysURL, passIssuer, audience, tenant, nil)
	}
	return srv, closeJournal, nil
}
