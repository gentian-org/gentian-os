/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// registrar keeps the list of people: it invites them, puts them in groups
// and removes them, at Keycloak, for the person entitled to ask
// (operator-split-plan.md §4.4).
//
// It is a process of its own, as the director and the custodian are, and for
// their reason. The director holds the credential that pushes to git; while
// it also held a Keycloak credential for every realm, one process could change
// both what the cluster runs and who may sign in to it. The registrar holds
// the Keycloak credential and no other: no git, no vault, and in the cluster
// only the right to read the tenants and the Cluster claim.
//
// It verifies a caller's token, asks the authorization store whether that
// person may manage the people of the tenant they named, and only then acts
// at Keycloak, as itself, recording whose act it was. It does not change who
// holds a platform role, whoever asks.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

func main() {
	ctrl.SetLogger(zap.New())
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("registrar stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// The registrar's own kinds and nothing else: it reads Tenants, and the
	// Cluster claim unstructured. No Secret and no ConfigMap is in this
	// scheme because none is read.
	scheme := runtime.NewScheme()
	utilruntime.Must(gentianov1alpha1.AddToScheme(scheme))

	// A manager for its client and its lifecycle, and nothing else: there is
	// no controller here, no webhook and no leader to elect. Two replicas of
	// the registrar are the same registrar.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	graph, err := store(log)
	if err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	srv, closeRecord, err := registrar.NewFromEnv(ctx, mgr.GetClient(), graph, log)
	if err != nil {
		return err
	}
	defer closeRecord()
	if err := mgr.Add(srv); err != nil {
		return err
	}
	log.Info("registrar listening", "addr", srv.Addr)
	return mgr.Start(ctx)
}

// store is the authorization store the operator established. The registrar
// reads it and never writes it; with none to ask it does not start, because
// something else deciding in its place is the one thing it must not allow.
func store(log *slog.Logger) (authz.Checker, error) {
	url := os.Getenv("OPENFGA_API_URL")
	if url == "" {
		return nil, fmt.Errorf("OPENFGA_API_URL is required: the authorization store decides who may manage people")
	}
	opts := authz.Options{
		BaseURL:  url,
		APIToken: os.Getenv("OPENFGA_API_TOKEN"),
		StoreID:  os.Getenv("OPENFGA_STORE_ID"),
		ModelID:  os.Getenv("OPENFGA_MODEL_ID"),
		Logger:   log,
	}
	if opts.StoreID == "" || opts.ModelID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		storeID, modelID, err := authz.Lookup(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("authorization store: %w", err)
		}
		if opts.StoreID == "" {
			opts.StoreID = storeID
		}
		if opts.ModelID == "" {
			opts.ModelID = modelID
		}
	}
	return authz.NewOpenFGA(opts)
}
