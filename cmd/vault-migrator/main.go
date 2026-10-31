/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// vault-migrator makes the vault what this release needs (internal/vaultmigrate).
//
// It runs from the operator's image with its own command, its own
// ServiceAccount and its own role at the vault, as a Job of the kernel chart.
// That role is composed by the Cluster composition, which the installer applies
// and the chart does not wait for, so a login refused now is tried again.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/vaultmigrate"
)

const (
	role     = "vault-migrator"
	attempts = 30
	pause    = 20 * time.Second
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	mariadb := layout.System("mariadb")
	gitops := layout.Kernel(layout.GitOps)
	names := []string{
		"mariadb." + mariadb + ".svc", "mariadb." + mariadb + ".svc.cluster.local",
		"argocd-server." + gitops + ".svc", "argocd-server." + gitops + ".svc.cluster.local",
	}
	vault := secrets.NewKVClient(os.Getenv("BAO_ADDR"), role, "")

	var err error
	for range attempts {
		err = vaultmigrate.Run(ctx, vault, secrets.Mount, names, vaultmigrate.Seeds, log.Info)
		if err == nil {
			log.Info("the vault is what this release needs")
			return
		}
		log.Warn("not yet", "error", err.Error())
		select {
		case <-ctx.Done():
			os.Exit(1)
		case <-time.After(pause):
		}
	}
	log.Error("the vault could not be brought up to this release", "error", err.Error())
	os.Exit(1)
}
