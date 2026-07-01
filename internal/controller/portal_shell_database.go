/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"net"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	portalShellAppName   = "shell"
	portalShellComponent = "portal-shell"
)

// ensurePortalShellDatabase provisions a per-tenant PostgreSQL database for Gentian
// shell persistence (user settings, audit events, notifications). It follows the
// same CNPG role Job + Database CR path as catalogue apps.
func (r *TenantReconciler) ensurePortalShellDatabase(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	nsName := tenantNamespaceName(tenant)
	dbName := databaseName(tenant, portalShellAppName)

	roleJobDone, err := r.ensureRoleJob(ctx, tenant, nsName, dbName, portalShellAppName)
	if err != nil {
		return false, fmt.Errorf("ensure portal shell role Job: %w", err)
	}
	if !roleJobDone {
		return false, nil
	}

	dbReady, err := r.ensureDatabaseCR(ctx, tenant, nsName, dbName, portalShellAppName)
	if err != nil {
		return false, fmt.Errorf("ensure portal shell Database CR: %w", err)
	}
	if !dbReady {
		return false, nil
	}

	conn := secrets.DatabaseCreds{
		Host: fmt.Sprintf("%s-rw.%s.svc.cluster.local", cnpgClusterName, kernelNamespace),
		Port: "5432",
		Name: dbName,
		User: roleUserName(tenant.Name, portalShellAppName),
	}
	if r.Seeder != nil {
		creds, seedErr := r.Seeder.SeedDatabase(ctx, tenant.Name, portalShellAppName, conn)
		if seedErr != nil {
			return false, fmt.Errorf("seed portal shell database: %w", seedErr)
		}
		conn = creds
	}

	if err := r.ensurePortalShellSecret(ctx, tenant, conn); err != nil {
		return false, err
	}
	return true, nil
}

func portalShellSecretName(tenantName string) string {
	return fmt.Sprintf("portal-shell-%s", tenantName)
}

func (r *TenantReconciler) ensurePortalShellSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, conn secrets.DatabaseCreds) error {
	databaseURL, err := portalShellDatabaseURL(conn)
	if err != nil {
		return err
	}
	name := portalShellSecretName(tenant.Name)
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                   tenant.Name,
				managedByLabel:                managedByValue,
				"app.kubernetes.io/name":        "gentian-portal",
				"app.kubernetes.io/component": portalShellComponent,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"host":         conn.Host,
			"port":         conn.Port,
			"database":     conn.Name,
			"username":     conn.User,
			"password":     conn.Password,
			"DATABASE_URL": databaseURL,
		},
	}
	existing := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.StringData = desired.StringData
	return r.Patch(ctx, existing, patch)
}

func portalShellDatabaseURL(conn secrets.DatabaseCreds) (string, error) {
	user := url.UserPassword(conn.User, conn.Password)
	u := &url.URL{
		Scheme: "postgresql+psycopg",
		User:   user,
		Host:   net.JoinHostPort(conn.Host, conn.Port),
		Path:   "/" + conn.Name,
	}
	return u.String(), nil
}
