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
		Name:     portalShellDBName,
		User:     portalShellRoleName,
		Password: "s3cr3t!",
	})
	if err != nil {
		t.Fatalf("portalShellDatabaseURL: %v", err)
	}
	want := "postgresql+psycopg://gentian_shell:s3cr3t%21@postgres-rw.platform-kernel.svc.cluster.local:5432/gentian_shell"
	if url != want {
		t.Fatalf("got %q want %q", url, want)
	}
}

func TestBuildPortalShellDatabaseCR(t *testing.T) {
	t.Parallel()
	cr := buildPortalShellDatabaseCR()
	if cr.GetName() != portalShellDatabaseCR {
		t.Fatalf("name = %q", cr.GetName())
	}
	if cr.GetNamespace() != kernelNamespace {
		t.Fatalf("namespace = %q", cr.GetNamespace())
	}
	name, found, err := unstructuredNestedString(cr.Object, "spec", "name")
	if err != nil || !found || name != portalShellDBName {
		t.Fatalf("spec.name = %q found=%v err=%v", name, found, err)
	}
}

func unstructuredNestedString(obj map[string]interface{}, fields ...string) (string, bool, error) {
	val, found, err := unstructuredNestedField(obj, fields...)
	if !found || err != nil {
		return "", found, err
	}
	s, ok := val.(string)
	return s, ok, nil
}

func unstructuredNestedField(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
	current := obj
	for i, field := range fields {
		if i == len(fields)-1 {
			val, ok := current[field]
			return val, ok, nil
		}
		next, ok := current[field].(map[string]interface{})
		if !ok {
			return nil, false, nil
		}
		current = next
	}
	return nil, false, nil
}
