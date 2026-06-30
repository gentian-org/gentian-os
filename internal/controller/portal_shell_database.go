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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	portalShellDBName     = "gentian_shell"
	portalShellRoleName   = "gentian_shell"
	portalShellDatabaseCR = "gentian-shell"
	portalShellRoleJob    = "provision-gentian-shell-role"
	portalShellSecret     = "gentian-portal-db"
	portalShellComponent  = "portal-shell"
)

// ensurePortalShellDatabase provisions the shared kernel PostgreSQL database used
// by gentian-portal-api for shell customization, admin audit events, and
// notifications. It runs during tenant database reconciliation so persistence
// is guaranteed before a tenant reaches Ready.
func (r *TenantReconciler) ensurePortalShellDatabase(ctx context.Context) (bool, error) {
	host := fmt.Sprintf("%s-rw.%s.svc.cluster.local", cnpgClusterName, kernelNamespace)
	conn := secrets.DatabaseCreds{
		Host: host,
		Port: "5432",
		Name: portalShellDBName,
		User: portalShellRoleName,
	}
	if r.Seeder != nil {
		creds, err := r.Seeder.SeedKernelDatabase(ctx, "portal-shell", conn)
		if err != nil {
			return false, fmt.Errorf("seed portal shell database: %w", err)
		}
		conn = creds
	}

	if err := r.ensurePortalShellRoleJob(ctx, conn.Password); err != nil {
		return false, err
	}
	roleDone, err := r.waitForProvisioningJob(ctx, "", portalShellRoleJob)
	if err != nil {
		return false, err
	}
	if !roleDone {
		return false, nil
	}

	if err := r.ensurePortalShellDatabaseCR(ctx); err != nil {
		return false, err
	}
	dbReady, err := r.portalShellDatabaseCRReady(ctx)
	if err != nil {
		return false, err
	}
	if !dbReady {
		return false, nil
	}

	if err := r.ensurePortalShellSecret(ctx, conn); err != nil {
		return false, err
	}
	return true, nil
}

func (r *TenantReconciler) ensurePortalShellRoleJob(ctx context.Context, rolePassword string) error {
	job := makePortalShellRoleJob(rolePassword)
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: portalShellRoleJob, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, job)
	}
	if err != nil {
		return err
	}
	if jobIsFailed(existing) {
		prop := metav1.DeletePropagationBackground
		if delErr := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &prop}); delErr != nil {
			return delErr
		}
		return r.Create(ctx, job)
	}
	return nil
}

func (r *TenantReconciler) ensurePortalShellDatabaseCR(ctx context.Context) error {
	desired := buildPortalShellDatabaseCR()
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind,
	})
	err := r.Get(ctx, types.NamespacedName{Name: portalShellDatabaseCR, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if cnpgDatabaseIsReady(existing) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.SetLabels(desired.GetLabels())
	if spec, found, specErr := unstructured.NestedMap(desired.Object, "spec"); specErr == nil && found {
		if setErr := unstructured.SetNestedField(existing.Object, spec, "spec"); setErr != nil {
			return setErr
		}
	}
	return r.Patch(ctx, existing, patch)
}

func (r *TenantReconciler) portalShellDatabaseCRReady(ctx context.Context) (bool, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind,
	})
	err := r.Get(ctx, types.NamespacedName{Name: portalShellDatabaseCR, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cnpgDatabaseIsReady(existing), nil
}

func (r *TenantReconciler) ensurePortalShellSecret(ctx context.Context, conn secrets.DatabaseCreds) error {
	databaseURL, err := portalShellDatabaseURL(conn)
	if err != nil {
		return err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      portalShellSecret,
			Namespace: kernelNamespace,
			Labels: map[string]string{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/name":    "gentian-portal",
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
	err = r.Get(ctx, types.NamespacedName{Name: portalShellSecret, Namespace: kernelNamespace}, existing)
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

func buildPortalShellDatabaseCR() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind,
	})
	obj.SetName(portalShellDatabaseCR)
	obj.SetNamespace(kernelNamespace)
	obj.SetLabels(map[string]string{
		managedByLabel:              managedByValue,
		"app.kubernetes.io/name":    "gentian-portal",
		"app.kubernetes.io/component": portalShellComponent,
	})
	_ = unstructured.SetNestedField(obj.Object, cnpgClusterName, "spec", "cluster", "name")
	_ = unstructured.SetNestedField(obj.Object, portalShellDBName, "spec", "name")
	_ = unstructured.SetNestedField(obj.Object, portalShellRoleName, "spec", "owner")
	_ = unstructured.SetNestedField(obj.Object, "present", "spec", "ensure")
	return obj
}

func makePortalShellRoleJob(rolePassword string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	container := psqlContainer("provision-role", buildRoleScript(portalShellDBName, portalShellRoleName), kernelNamespace)
	if rolePassword != "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "ROLE_PW",
			Value: rolePassword,
		})
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      portalShellRoleJob,
			Namespace: kernelNamespace,
			Labels: map[string]string{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/name":    "gentian-portal",
				"app.kubernetes.io/component": portalShellComponent,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
	return job
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
