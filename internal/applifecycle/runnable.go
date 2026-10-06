/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/licencereport"
)

// Runnable serves the app lifecycle HTTP API inside the operator manager.
type Runnable struct {
	Server *HTTPServer
}

// NewRunnableFromEnv builds the lifecycle HTTP server from operator environment.
func NewRunnableFromEnv(mgr manager.Manager) (*Runnable, error) {
	addr := os.Getenv("APP_LIFECYCLE_BIND_ADDRESS")
	if addr == "" {
		addr = ":8082"
	}
	namespace := envOrDefault("POD_NAMESPACE", layout.Namespace(layout.Control))
	svc, err := NewService(mgr.GetClient(), mgr.GetConfig(), Options{
		OpenBaoNamespace:  envOrDefault("OPENBAO_NAMESPACE", "openbao"),
		OperatorNamespace: namespace,
		OperatorSA:        envOrDefault("OPERATOR_SA", "gentian-os"),
		MetricsEnabled:    os.Getenv("METRICS_SERVER_ENABLED") == "true",
		LicenceReport:     licencereport.SettingsFromEnv(),
	})
	if err != nil {
		return nil, err
	}
	// Who is admitted: the director's and the usher's ServiceAccounts, in
	// this namespace, presenting a token issued for this listener's
	// audience. Anything missing admits fewer callers, never more; with
	// nothing configured the server refuses every request.
	return &Runnable{
		Server: &HTTPServer{
			Service: svc,
			Addr:    addr,
			Auth:    NewCallerAuth(svc.clientset.AuthenticationV1().TokenReviews(), CallersFromEnv(namespace)),
		},
	}, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Start implements manager.Runnable.
func (r *Runnable) Start(ctx context.Context) error {
	return r.Server.Start(ctx)
}

// NeedLeaderElection returns false so any operator replica can serve read-mostly API calls.
func (r *Runnable) NeedLeaderElection() bool { return false }
