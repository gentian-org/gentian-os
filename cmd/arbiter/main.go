/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// arbiter is the one way into the app instance of OpenFGA
// (internal/arbiter): every component that declares an authorization store
// asks it, with a key the arbiter knows only by its hash, and is answered
// from its own store and nothing else.
//
// It runs in the system namespace beside the instance, from the operator's
// image with its own command and its own ServiceAccount, which may read and
// write two objects of that namespace: the record of the stores it created,
// and the instance's key, which it makes on its first start and nobody else
// is handed.
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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gentian-org/gentian-os/internal/arbiter"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("arbiter stopped", "error", err.Error())
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
	namespace := envOr("POD_NAMESPACE", arbiter.Namespace)
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	key, err := arbiter.EnsureInstanceKey(ctx, kube, namespace)
	if err != nil {
		return err
	}
	fga := arbiter.NewOpenFGA(envOr("OPENFGA_API_URL",
		"http://"+arbiter.InstanceRelease+"."+namespace+".svc.cluster.local:8080"), key)
	stores := arbiter.NewStores(fga, &arbiter.ConfigMapRecorder{Client: kube, Namespace: namespace}, log)
	srv := arbiter.NewServer(fga, stores, log)
	go arbiter.Follow(ctx, envOr("ARBITER_TABLE", "/etc/arbiter/"+arbiter.TableKey), 5*time.Second, srv, log)

	health := &http.Server{Addr: envOr("ARBITER_HEALTH", ":8081"), ReadHeaderTimeout: 5 * time.Second}
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	go func() { _ = health.ListenAndServe() }()

	// The platform's path: the operator's ServiceAccount, by the API
	// server's word on a token projected for this path alone.
	platform := &arbiter.PlatformAuth{Reviews: kube.AuthenticationV1().TokenReviews(), Caller: os.Getenv("ARBITER_PLATFORM_CALLER")}
	api := &http.Server{
		Addr: envOr("ARBITER_LISTEN", ":8080"), Handler: srv.Handler(platform),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- api.ListenAndServe() }()
	log.Info("arbiter listening", "addr", api.Addr, "namespace", namespace)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = api.Shutdown(shutdown)
	_ = health.Shutdown(shutdown)
	return nil
}
