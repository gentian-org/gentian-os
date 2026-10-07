/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// custodian takes a credential from the person entitled to set it and puts it
// in the vault (operator-split-plan.md §4.3).
//
// It is a process of its own, as the director is, and for the director's
// reason. The director is the only thing that holds the credential to push to
// git; the custodian is the only thing that holds the identity that sets a
// credential, and that identity can write one and cannot read one. Run inside
// the operator, it shared a ServiceAccount with a process whose own vault role
// reads every secret the platform has, which made the narrow role a statement
// of intent and nothing more.
//
// It verifies a caller's token, asks the authorization store whether that
// person may see or set the credentials of the cluster or of their tenant,
// and only then touches the vault, as itself.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/custodian"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

func main() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("custodian")
	if err := run(); err != nil {
		log.Error(err, "custodian stopped")
		os.Exit(1)
	}
}

func run() error {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gentianov1alpha1.AddToScheme(scheme))

	// A manager for its client and its lifecycle, and nothing else: there is
	// no controller here, no webhook and no leader to elect. Two replicas of
	// the custodian are the same custodian.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Client: client.Options{Cache: &client.CacheOptions{
			// Read where they are named, one object at a time. A cached read
			// would need to list and watch every ConfigMap and Secret in the
			// cluster, which is a great deal more than the one of each this
			// service is allowed.
			DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}},
		}},
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	graph, err := store()
	if err != nil {
		return err
	}
	keeper, err := custodian.NewRunnableFromEnv(mgr, custodian.NewEndpointValidator(), graph)
	if err != nil {
		return err
	}
	if err := mgr.Add(keeper); err != nil {
		return err
	}
	ctrl.Log.WithName("custodian").Info("custodian API enabled", "addr", keeper.Addr)
	return mgr.Start(ctrl.SetupSignalHandler())
}

// store is the authorization store the operator established. The custodian
// reads it and never writes it; with none to ask it does not start, because
// something else deciding in its place is the one thing it must not allow.
func store() (authz.Checker, error) {
	url := os.Getenv("OPENFGA_API_URL")
	if url == "" {
		return nil, fmt.Errorf("OPENFGA_API_URL is required: the authorization store decides who may touch a credential")
	}
	opts := authz.Options{
		BaseURL:  url,
		APIToken: os.Getenv("OPENFGA_API_TOKEN"),
		StoreID:  os.Getenv("OPENFGA_STORE_ID"),
		ModelID:  os.Getenv("OPENFGA_MODEL_ID"),
		Logger:   slog.New(slog.NewJSONHandler(os.Stderr, nil)),
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
