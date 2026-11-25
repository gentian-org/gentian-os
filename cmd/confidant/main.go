/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// The confidant: what keeps secrets of a person for the components that act
// for them (internal/confidant). It reads nothing from the cluster: the
// components it serves are in a list it mounts, and its one credential is
// its own ServiceAccount token, which the vault takes for a role that
// reaches people's secrets and nothing else.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gentian-org/gentian-os/internal/confidant"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/envvar"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "confidant")
	if err := run(log); err != nil {
		log.Error("confidant stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	issuer := os.Getenv("CONFIDANT_ISSUER_BASE_URL")
	if issuer == "" {
		return fmt.Errorf("the confidant needs CONFIDANT_ISSUER_BASE_URL to verify a person's token")
	}
	// No audience of its own: a person's token is verified for the component
	// that presents it, one request at a time (VerifyFor).
	verifier, err := authn.NewVerifier(authn.Config{
		IssuerBase: issuer,
		JWKSBase:   os.Getenv("CONFIDANT_JWKS_BASE_URL"),
		Audience:   "gentian-confidant",
	})
	if err != nil {
		return err
	}
	addr := os.Getenv("BAO_ADDR")
	if addr == "" {
		return fmt.Errorf("the confidant needs BAO_ADDR: the vault is where it keeps what it is given")
	}
	vault := secrets.NewKVClient(addr, envvar.Or("CONFIDANT_BAO_ROLE", "gentian-os-confidant"), "")

	keys := &confidant.Keys{}
	go keys.Follow(ctx, envvar.Or("CONFIDANT_KEYS", "/etc/gentian/confidant/keys.json"), 5*time.Second, log, confidant.KeysList)

	srv, err := confidant.New(confidant.Config{Keys: keys, Tokens: verifier, Vault: vault, Log: log})
	if err != nil {
		return err
	}
	srv.Addr = envvar.Or("CONFIDANT_ADDR", srv.Addr)
	log.Info("confidant listening", "addr", srv.Addr)
	return srv.Start(ctx)
}
