/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Command porter is the platform's file dialog (internal/porter). One runs
// per tenant, behind the edge's session.
//
// Settings, all from the environment:
//
//	PLAN_FILE     what it serves, as the platform writes it
//	VOUCHING_DIR  the Secret vouching-<profile> mounted, one file per key,
//	              read each time a person's token for the home is asked for
//	LISTEN        where it listens (default :8080)
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

	"github.com/gentian-org/gentian-os/internal/porter"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("porter stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plan, vouching := os.Getenv("PLAN_FILE"), os.Getenv("VOUCHING_DIR")
	if plan == "" {
		return errors.New("PLAN_FILE is not set")
	}
	if vouching == "" {
		return errors.New("VOUCHING_DIR is not set")
	}
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	if _, err := porter.ReadPlan(plan); err != nil {
		log.Warn("the plan cannot be read yet; nobody is served until it can", "error", err.Error())
	}
	server := &porter.Server{
		Plan:  func() (*porter.Plan, error) { return porter.ReadPlan(plan) },
		Homes: porter.NewHomes(vouching),
		Log:   log,
	}
	srv := &http.Server{Addr: listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	log.Info("porter serves", "listen", listen)
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
