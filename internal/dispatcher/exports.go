/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package dispatcher

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ClusterExports writes the records of backups in the cluster: a
// TenantExport in the tenant's namespace. The operator's reconciler takes
// the backup from the record, and deletes the bundle when the record goes.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports,verbs=create;delete
type ClusterExports struct {
	Client client.Client
}

// namespace is where a tenant's records are kept, read off the Tenant: a
// tenant that is not there has none.
func (e ClusterExports) namespace(ctx context.Context, tenant string) (string, error) {
	t := &gentianov1alpha1.Tenant{}
	if err := e.Client.Get(ctx, types.NamespacedName{Name: tenant}, t); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("tenant %q: %w", tenant, ErrNoSuch)
		}
		return "", err
	}
	return t.NamespaceName(), nil
}

// Start writes the record of one backup.
func (e ClusterExports) Start(ctx context.Context, tenant string, backup Backup, actor string) error {
	namespace, err := e.namespace(ctx, tenant)
	if err != nil {
		return err
	}
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{
			Name: backup.Name, Namespace: namespace,
			// Who asked. The reconciler does not read it; a person looking
			// at a bundle months later does.
			Annotations: map[string]string{gentianov1alpha1.RequestedByAnnotation: actor},
		},
		Spec: gentianov1alpha1.TenantExportSpec{Apps: backup.Apps, Destination: backup.Destination},
	}
	if len(backup.Recipients) > 0 {
		export.Spec.Encryption = &gentianov1alpha1.ExportEncryption{
			Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: backup.Recipients,
		}
	}
	if err := e.Client.Create(ctx, export); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("a backup named %q of tenant %q: %w", backup.Name, tenant, ErrExists)
		}
		return err
	}
	return nil
}

// Delete removes the record of one backup.
func (e ClusterExports) Delete(ctx context.Context, tenant, name string) error {
	namespace, err := e.namespace(ctx, tenant)
	if err != nil {
		return err
	}
	export := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := e.Client.Delete(ctx, export); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("backup %q of tenant %q: %w", name, tenant, ErrNoSuch)
		}
		return err
	}
	return nil
}
