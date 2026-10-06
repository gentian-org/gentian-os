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
	"github.com/gentian-org/gentian-os/internal/layout"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/manager"
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
	svc, err := NewService(mgr.GetClient(), mgr.GetConfig(), Options{
		OpenBaoNamespace:  envOrDefault("OPENBAO_NAMESPACE", "openbao"),
		OperatorNamespace: envOrDefault("POD_NAMESPACE", layout.Namespace(layout.Control)),
		OperatorSA:        envOrDefault("OPERATOR_SA", "gentian-os"),
		MetricsEnabled:    os.Getenv("METRICS_SERVER_ENABLED") == "true",
	})
	if err != nil {
		return nil, err
	}
	// The shared token the director presents. Absent means the server
	// refuses every request: an operator whose Secret failed to mount must
	// not fall back to the open API this replaced.
	return &Runnable{Server: &HTTPServer{
		Service: svc,
		Addr:    addr,
		Token:   os.Getenv("APP_LIFECYCLE_TOKEN"),
	}}, nil
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
