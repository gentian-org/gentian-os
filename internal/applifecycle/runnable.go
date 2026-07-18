/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package applifecycle

import (
	"context"
	"os"

	"github.com/gentian-org/gentian-os/internal/meta"
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
		KernelNamespace:    envOrDefault("KERNEL_NAMESPACE", meta.KernelNamespace),
		OpenBaoNamespace:   envOrDefault("OPENBAO_NAMESPACE", "openbao"),
		OperatorNamespace:  envOrDefault("POD_NAMESPACE", "gentian-system"),
		OperatorSA:         envOrDefault("OPERATOR_SA", "gentian-os"),
		DeploymentsPath:    os.Getenv("GENTIAN_DEPLOYMENTS_PATH"),
		DeploymentsRepo:    os.Getenv("GENTIAN_DEPLOYMENTS_REPO"),
		DeploymentsCluster: envOrDefault("GENTIAN_DEPLOYMENTS_CLUSTER", "default-cluster"),
	})
	if err != nil {
		return nil, err
	}
	return &Runnable{Server: &HTTPServer{Service: svc, Addr: addr}}, nil
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
