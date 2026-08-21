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

package controller

import (
	"context"
	"fmt"

	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/oidc"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	dovecotTenantOIDCClientVersion = "3"
	dovecotOIDCClientID            = "gentian-dovecot"

	// dovecotAdminSecretName carries doveadm_password and oidc_client_secret,
	// synced from gentian-os/kernel/mail/dovecot in OpenBao.
	//
	// Note the duplication: the Dovecot chart's own ExternalSecret
	// (dovecot-sensitive-values) pulls the same two properties from the same path
	// into the same namespace under a different name. Both work; they should
	// become one.
	dovecotAdminSecretName = "dovecot-admin"
)

// No tenant-realm job name any more: tenant-default composes that client, and
// the operator waits for it. The kernel-realm one below stays, because no
// Composition covers the kernel realm — there is no XTenant for it.
//
// Any keycloak-dovecot-oidc-<tenant> Job left on a cluster expires on its own:
// the Jobs carry TTLSecondsAfterFinished, so a completed one is collected
// without anything having to go and delete it.

// kernelDovecotOIDCClientJobName is not tenant-scoped: there is one kernel realm
// and one client in it, however many tenants exist.
func kernelDovecotOIDCClientJobName() string {
	return "keycloak-dovecot-oidc-kernel"
}

// makeDovecotOIDCClientJob provisions gentian-dovecot in one Keycloak realm.
//
// Dovecot introspects IMAP XOAUTH2 tokens in the realm that ISSUED them, and the
// introspection endpoint authenticates its caller, so every realm whose users have
// mailboxes needs this client. That includes the kernel realm, where the cluster
// admin lives, not only tenant realms.
//
// The script comes from buildOIDCPackScript, the same builder every other client
// in the platform goes through. This used to carry its own hand-written copy of
// the Keycloak calls, which drifted from the pack path for no reason other than
// having been written separately: it built precisely the serviceClient shape the
// pack mechanism now expresses.
//
// The secret is injected by reference, never as a Job env Value. makeOIDCPackJob
// passes app client secrets literally, which puts them in a Job spec readable by
// anyone with get on Jobs in the kernel namespace; this path keeps the secretKeyRef
// the hand-rolled version had.
func makeDovecotOIDCClientJob(jobName, realmName string, pack oidc.Pack, labels map[string]string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	c := keycloakContainer("dovecot-oidc-client",
		buildOIDCPackScript(realmName, dovecotOIDCClientID, pack, nil, nil, "", ""))
	c.Env = append(c.Env, corev1.EnvVar{
		Name: "OIDC_CLIENT_SECRET",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: dovecotAdminSecretName},
				Key:                  "oidc_client_secret",
			},
		},
	})
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: kernelNamespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{c},
				},
			},
		},
	}
}

// resolveDovecotPack reads the gentian-dovecot service pack from the kernel
// OIDCPackCatalog shipped in the operator chart.
//
// A missing catalogue is reported as not-found rather than an error, so the caller
// requeues instead of failing the tenant. The catalogue is a chart object that Argo
// CD may not have synced when the operator first reconciles a tenant, and treating
// startup ordering as an error blocks the whole tenant — mail included — behind a
// condition that resolves itself seconds later.
//
// A pack that exists but is not a serviceClient IS an error: that is a mistake in
// the catalogue, not a race, and provisioning it as an app client would put a
// pointless scope and role in every realm.
func (r *TenantReconciler) resolveDovecotPack(ctx context.Context) (oidc.Pack, bool, error) {
	pack, _, ok, err := oidc.ResolvePack(ctx, r.Client, dovecotOIDCClientID)
	if err != nil {
		return oidc.Pack{}, false, fmt.Errorf("resolve %s pack: %w", dovecotOIDCClientID, err)
	}
	if !ok {
		return oidc.Pack{}, false, nil
	}
	if !pack.ServiceClient {
		return oidc.Pack{}, false, fmt.Errorf("pack %q must set serviceClient: Dovecot needs a confidential client with no scope or entitlement group", dovecotOIDCClientID)
	}
	return pack, true, nil
}

// keycloakClientGVK is the provider-keycloak managed resource that tenant-default
// composes for gentian-dovecot.
var keycloakClientGVK = schema.GroupVersionKind{
	Group:   "openidclient.keycloak.crossplane.io",
	Version: "v1alpha1",
	Kind:    "Client",
}

func tenantDovecotOIDCClientName(tenantName string) string {
	return fmt.Sprintf("%s-dovecot-oidc-client", tenantName)
}

// dovecotTenantClientReady reports whether the composed gentian-dovecot Client is
// Ready in the tenant realm.
//
// It replaces a Job that created the same client. The object is tenant-default's
// now — declared with the secret from dovecot-admin, because an omitted
// clientSecretSecretRef would have Keycloak generate a new one and IMAP would
// stop validating tokens — so all that is left here is to wait for it.
//
// The wait itself has to stay. Steps 6 and 7 add the realm to Dovecot's XOAUTH2
// configuration and reload it, and introspection against a client that does not
// exist yet fails; deleting the wait along with the Job broke three readiness
// tests for exactly that reason.
func (r *TenantReconciler) dovecotTenantClientReady(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(keycloakClientGVK)
	err := r.Get(ctx, types.NamespacedName{Name: tenantDovecotOIDCClientName(tenant.Name)}, obj)
	switch {
	case errors.IsNotFound(err):
		return false, nil
	case apimeta.IsNoMatchError(err):
		// provider-keycloak is not installed yet. A cluster still bootstrapping
		// should wait for it rather than fail the whole mail reconcile.
		log.FromContext(ctx).Info("waiting for the provider-keycloak Client CRD before configuring Dovecot",
			"client", dovecotOIDCClientID)
		return false, nil
	case err != nil:
		return false, err
	}
	return appClaimIsReady(obj), nil
}

// ensureKernelDovecotOIDCClientJob provisions the client in the KERNEL realm, so
// the cluster admin has a working mailbox on a cluster with no tenants at all.
func (r *TenantReconciler) ensureKernelDovecotOIDCClientJob(ctx context.Context) (bool, error) {
	if r.KernelRealm == "" {
		return false, fmt.Errorf("KERNEL_REALM is empty; cannot provision %s in the kernel realm", dovecotOIDCClientID)
	}
	pack, ok, err := r.resolveDovecotPack(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		log.FromContext(ctx).Info("waiting for the kernel OIDCPackCatalog before provisioning the kernel-realm Dovecot client",
			"pack", dovecotOIDCClientID)
		return false, nil
	}
	jobName := kernelDovecotOIDCClientJobName()
	job := makeDovecotOIDCClientJob(jobName, r.KernelRealm, pack, map[string]string{
		managedByLabel: managedByValue,
		"gentianos.io/keycloak-dovecot-oidc-client": dovecotTenantOIDCClientVersion,
	})
	return r.ensureDovecotOIDCClientJob(ctx, "", jobName, job)
}

func (r *TenantReconciler) ensureDovecotOIDCClientJob(ctx context.Context, tenantName, jobName string, desired *batchv1.Job) (bool, error) {
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenantName, jobName)
}
