/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// The liaison: keeps a person's connections to services outside the cluster
// (internal/liaison).
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

	"github.com/gentian-org/gentian-os/internal/liaison"
	"github.com/gentian-org/gentian-os/internal/liaison/auth"
)

const address = ":8000"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "liaison")
	if err := run(log); err != nil {
		log.Error("liaison stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The confidant's address and the liaison's key for it are the
	// platform's, in the Secret it keeps for a component that declares
	// requires.services.personSecrets.
	confidantURL, key := os.Getenv("CONFIDANT_URL"), os.Getenv("RIGHTS_CHECK_KEY")
	var confidant liaison.Confidant = liaison.NoConfidant{}
	if confidantURL != "" && key != "" {
		confidant = liaison.NewConfidantClient(confidantURL, key, nil)
	} else {
		log.Warn("CONFIDANT_URL or RIGHTS_CHECK_KEY is not set: this platform keeps no secrets of people, so nobody can connect")
	}
	issuer, audience := os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_OWN_AUDIENCE")
	if issuer == "" || audience == "" {
		return errors.New("the liaison needs OIDC_ISSUER and OIDC_OWN_AUDIENCE to know who is at its page")
	}
	chaperoneKey := os.Getenv("CHAPERONE_KEY")
	if chaperoneKey == "" {
		log.Warn("CHAPERONE_KEY is not set: the chaperone is not accepted, so no connection is known and no credential is handed out")
	}
	name := os.Getenv("CLIENT_NAME")
	if name == "" {
		name = "Gentian workspace"
	}

	service := liaison.NewService(liaison.NewOAuth(nil), confidant, name)
	server := liaison.NewServer(service, auth.NewOIDC(issuer, audience, nil), chaperoneKey, log)

	srv := &http.Server{Addr: address, Handler: server, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Info("liaison listening", "addr", address)
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	case err := <-done:
		return err
	}
}
