/*
Copyright 2026 The Gentian Authors.
*/

package controller

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

func TestPortalShellDatabaseURL(t *testing.T) {
	t.Parallel()
	url, err := portalShellDatabaseURL(secrets.DatabaseCreds{
		Host:     "postgres-rw.platform-kernel.svc.cluster.local",
		Port:     "5432",
		Name:     "demo_shell",
		User:     "demo_shell",
		Password: "s3cr3t!",
	})
	if err != nil {
		t.Fatalf("portalShellDatabaseURL: %v", err)
	}
	want := "postgresql+psycopg://demo_shell:s3cr3t%21@postgres-rw.platform-kernel.svc.cluster.local:5432/demo_shell"
	if url != want {
		t.Fatalf("got %q want %q", url, want)
	}
}

func TestPortalShellSecretName(t *testing.T) {
	t.Parallel()
	if got := portalShellSecretName("demo"); got != "portal-shell-demo" {
		t.Fatalf("got %q", got)
	}
}
