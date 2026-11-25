/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	tenantProvisioningJobsConfigType = "tenant-provisioning-jobs"
	tenantProvisioningJobsDataKey    = "jobs.json"
)

func tenantProvisioningConfigMapName(tenantName string) string {
	return "tenant-" + tenantName + "-provisioning-jobs"
}

// ensureTenantProvisioningManifests writes Batch Job and Kubernetes object manifests
// that Crossplane applies via tenant-default. The operator seeds
// credentials and updates this ConfigMap; it does not create those resources directly.
func (r *TenantReconciler) ensureTenantProvisioningManifests(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	jobs, keys, err := r.buildTenantProvisioningJobs(ctx, tenant)
	if err != nil {
		return fmt.Errorf("build tenant provisioning jobs: %w", err)
	}
	// The passwords and key pairs the Jobs provision the apps' stores with,
	// and the apps' OIDC client secrets, each in a Secret its Job owns: never
	// in the manifests below.
	if err := r.ensureProvisioningJobSecrets(ctx, keys); err != nil {
		return err
	}
	// On record before anything is made: see recordProvisionedStores.
	if err := r.recordProvisionedStores(ctx, tenant); err != nil {
		return err
	}
	keepFinishedJobs(jobs)
	stampScriptHashes(jobs)
	// A Job's pod template is immutable, so re-applying a changed script does
	// nothing to a Job that already ran: the old script stays the last thing
	// that touched the cluster, silently. Retire the finished ones whose script
	// has changed and let Crossplane recreate them from the manifests below.
	r.retireStaleProvisioningJobs(ctx, jobs)
	jobsPayload, err := serializeProvisioningJobs(jobs)
	if err != nil {
		return fmt.Errorf("serialize tenant provisioning jobs: %w", err)
	}

	objects, err := r.buildTenantProvisioningObjects(ctx, tenant)
	if err != nil {
		return fmt.Errorf("build tenant provisioning objects: %w", err)
	}
	objectsPayload, err := serializeProvisioningObjects(objects)
	if err != nil {
		return fmt.Errorf("serialize tenant provisioning objects: %w", err)
	}

	name := tenantProvisioningConfigMapName(tenant.Name)
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: provisioningNamespace,
			Labels: map[string]string{
				tenantLabel:                tenant.Name,
				"gentianos.io/config-type": tenantProvisioningJobsConfigType,
				managedByLabel:             managedByValue,
			},
		},
		Data: map[string]string{
			tenantProvisioningJobsDataKey:    jobsPayload,
			tenantProvisioningObjectsDataKey: objectsPayload,
		},
	}

	return ensureObject(ctx, r.Client, desired, func(existing *corev1.ConfigMap) bool {
		if existing.Data != nil &&
			existing.Data[tenantProvisioningJobsDataKey] == jobsPayload &&
			existing.Data[tenantProvisioningObjectsDataKey] == objectsPayload {
			return false
		}
		if existing.Data == nil {
			existing.Data = map[string]string{}
		}
		existing.Data[tenantProvisioningJobsDataKey] = jobsPayload
		existing.Data[tenantProvisioningObjectsDataKey] = objectsPayload
		existing.Labels = desired.Labels
		return true
	})
}

func (r *TenantReconciler) buildTenantProvisioningObjects(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]client.Object, error) {
	return r.buildDataPlaneObjects(ctx, tenant)
}

func serializeProvisioningJobs(jobs []batchv1.Job) (string, error) {
	clean := make([]batchv1.Job, 0, len(jobs))
	for i := range jobs {
		clean = append(clean, sanitizeJobForJSON(&jobs[i]))
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func sanitizeJobForJSON(job *batchv1.Job) batchv1.Job {
	out := job.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"}
	out.ResourceVersion = ""
	out.UID = ""
	out.Generation = 0
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.Status = batchv1.JobStatus{}
	out.Spec.Selector = nil
	out.Spec.Template.ResourceVersion = ""
	return *out
}

func (r *TenantReconciler) buildTenantProvisioningJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]batchv1.Job, provisioningJobSecrets, error) {
	var jobs []batchv1.Job
	realmName := keycloakRealmName(tenant)

	identityJobs, clientSecrets, err := r.buildIdentityProvisioningJobs(ctx, tenant, realmName)
	if err != nil {
		return nil, nil, err
	}
	jobs = append(jobs, identityJobs...)

	dataJobs, keys, err := r.buildDataPlaneJobs(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	jobs = append(jobs, dataJobs...)
	for job, data := range clientSecrets {
		keys[job] = data
	}
	return jobs, keys, nil
}

// buildIdentityProvisioningJobs builds the Jobs that provision a tenant's
// realm and its apps' clients, and the OIDC client secrets those Jobs are to
// be handed, by Job (ensureProvisioningJobSecrets): those are not in the Jobs.
func (r *TenantReconciler) buildIdentityProvisioningJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) ([]batchv1.Job, provisioningJobSecrets, error) {
	var jobs []batchv1.Job
	clientSecrets := provisioningJobSecrets{}

	// A tenant that adopts the kernel realm gets no realm Job, no
	// administrator, no broker and no mail server: those are the realm's,
	// and the realm is the identity bootstrap's. See adoptsKernelRealm.
	adopted := r.adoptsKernelRealm(tenant)
	if !adopted {
		jobs = append(jobs, *makeRealmJob(tenant, realmName))
	}

	// An app whose client is refused gets no Job; ensureIdentity says why.
	oidcConfigs, _, err := r.collectOIDCAppConfigs(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}

	samlConfigs, err := r.collectSAMLAppConfigs(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}

	jobs = append(jobs, *makeGentianGroupsJob(tenant, realmName))

	if !adopted {
		jobs = append(jobs, *makeAdminJob(tenant, realmName, r.tenantAdminUsername(tenant)))
	}

	for _, cfg := range oidcConfigs {
		profile, err := r.getOIDCOwnerProfile(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		if crossplaneOwnsOIDCClient(profile, cfg) {
			if err := r.seedOIDCSecrets(ctx, tenant, realmName, cfg); err != nil {
				return nil, nil, err
			}
			continue
		}
		job, clientSecret, err := r.buildOIDCClientProvisioningJob(ctx, tenant, realmName, cfg)
		if err != nil {
			return nil, nil, err
		}
		if clientSecret != "" {
			clientSecrets[types.NamespacedName{Namespace: job.Namespace, Name: job.Name}] = clientSecretSecretData(clientSecret)
		}
		jobs = append(jobs, *job)
	}

	for _, cfg := range samlConfigs {
		jobs = append(jobs, *makeSAMLClientJob(tenant, realmName, cfg.profileName, cfg.entityID, cfg.acsURL))
	}

	if r.KernelRealm != "" && r.KernelDomain != "" && !adopted {
		// No broker Jobs. The bridges between a tenant realm and the kernel
		// realm, in either direction, are the Compositions' (hub_projection.go).
		// No portal clients at all: the portal is retired, and a tenant's people
		// sign in through the edge's zone client, which tenant-default composes.
		// The Job writes the mail server the Composition declares, password
		// included, so it is built exactly where one is declared: a realm
		// that is to have no mail server gets no Job that would give it one.
		realmMail, err := r.tenantRealmMail(ctx, tenant)
		if err != nil {
			return nil, nil, fmt.Errorf("the realm's mail server: %w", err)
		}
		if realmMail != nil && r.clusterKeycloakSMTPCredentialsAvailable(ctx) {
			mailHost := ""
			if r.KernelDomain != "" {
				mailHost = "mail." + r.KernelDomain
			}
			jobs = append(jobs, *makeTenantSMTPJob(tenant.Name, realmName, mailHost, brandName(ctx, r.Client)))
		}
	}

	return jobs, clientSecrets, nil
}

// tenantAdminUsername is the administrator's login. The account has no
// password until its holder sets one through an activation link: nothing here
// derives, stores or logs one.
func (r *TenantReconciler) tenantAdminUsername(tenant *gentianov1alpha1.Tenant) string {
	return tenant.TenantAdminUsername(r.KernelDomain, r.TenancyMode)
}

// seedOIDCSecrets writes issuer/client-id/client-secret to OpenBao for apps whose
// Keycloak Client MR is owned by Crossplane (operator OIDC Jobs are skipped).
func (r *TenantReconciler) seedOIDCSecrets(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) error {
	if r.Seeder == nil || cfg.clientID == "" {
		return nil
	}
	issuer := fmt.Sprintf("https://id.%s/auth/realms/%s", r.KernelDomain, realmName)
	if _, err := r.Seeder.SeedOIDC(ctx, tenant.Name, cfg.profileName, issuer, cfg.clientID); err != nil {
		return fmt.Errorf("seed oidc for %s: %w", cfg.profileName, err)
	}
	return nil
}

// buildOIDCClientProvisioningJob builds the Job that makes an app's OIDC
// client, and the client secret it is to be handed in the Secret it owns
// (empty when it is handed none).
func (r *TenantReconciler) buildOIDCClientProvisioningJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) (*batchv1.Job, string, error) {
	if cfg.pack != nil {
		clientSecret := ""
		if r.Seeder != nil && !cfg.pack.PublicClient {
			issuer := fmt.Sprintf("https://id.%s/auth/realms/%s", r.KernelDomain, realmName)
			creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, cfg.profileName, issuer, cfg.clientID)
			if seedErr != nil {
				return nil, "", fmt.Errorf("seed oidc pack for %s: %w", cfg.profileName, seedErr)
			}
			clientSecret = creds.ClientSecret
		}
		return makeOIDCPackJob(tenant, realmName, cfg, clientSecret != ""), clientSecret, nil
	}

	clientSecret := ""
	if r.Seeder != nil {
		issuer := fmt.Sprintf("https://id.%s/auth/realms/%s", r.KernelDomain, realmName)
		creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, cfg.profileName, issuer, cfg.clientID)
		if seedErr != nil {
			return nil, "", fmt.Errorf("seed oidc for %s: %w", cfg.profileName, seedErr)
		}
		clientSecret = creds.ClientSecret
	}
	return makeClientJob(tenant, realmName, cfg.profileName, cfg.clientID, cfg.redirectURIs, clientSecret != ""), clientSecret, nil
}

// crossplaneOwnsOIDCClient reports whether the app's release already comes
// with a provider-keycloak Client MR, rendered by the operator (apprender) or
// by a profile's own Composition, so the operator pack/client Job can be
// skipped. That is every client without an OIDC pack of an app that installs
// a chart: the app's own (requires.services.identity.oidc) and each
// extension's (extensions[].kernelRequirements.identity.oidc), which is
// rendered beside it. A client with a pack is the pack Job's; an extension's
// request-bound client is refused before it gets here
// (resolveSidecarOIDCAppConfig).
func crossplaneOwnsOIDCClient(profile *gentianov1alpha1.ComponentProfile, cfg oidcAppConfig) bool {
	if cfg.pack != nil {
		return false
	}
	// An ApiProfile has no App claim and therefore no Composition to own a Client
	// MR. This was written as "anything that is not crossplane", which also
	// covered deploymentMethod: argocd — a value no profile ever set and that no
	// longer exists.
	if profile.IsAPI() {
		return false
	}
	// An extension's client is declared on the parent profile, and is
	// rendered with the parent's release, whoever renders it.
	if cfg.parentProfile != "" {
		return true
	}
	kr := profile.Services()
	if kr == nil || kr.Identity == nil || kr.Identity.OIDC == nil {
		return false
	}
	return true
}

// provisioningScriptHashAnnotation records which script a provisioning Job was
// built from, so a finished Job can be told apart from one whose desired script
// has since changed.
const provisioningScriptHashAnnotation = "gentianos.io/script-hash"

// keepFinishedJobs takes the TTL off Jobs Crossplane is to hold.
//
// Each of these Jobs is the manifest of a provider-kubernetes Object, and an
// Object recreates whatever it holds the moment it finds it missing. A TTL
// therefore never retired one of them: Kubernetes deleted the finished Job,
// the Object made it again, and the script ran again — every TTL, forever,
// against a Keycloak or store that had long been set up. A finished Job now
// stays, as the record that its step ran, and runs again only when what it
// runs changes (retireStaleProvisioningJobs) or it failed
// (waitForProvisioningJob). The builders keep their TTL for the Jobs the
// operator makes itself, which nothing recreates.
func keepFinishedJobs(jobs []batchv1.Job) {
	for i := range jobs {
		jobs[i].Spec.TTLSecondsAfterFinished = nil
	}
}

// scriptHash digests what a Job runs: its whole pod template — image, command,
// arguments, and env, which is how a Job is handed its inputs (the groups a
// realm is to have, whether its administrator needs a second factor). A
// changed input is a changed hash, and the Job runs again with it.
//
// Nothing in a pod template is generated per reconcile: a credential reaches a
// provisioning Job as a reference to the Secret its Job owns (jobSecret), never
// as a value, so the hash is stable for as long as the inputs are.
func scriptHash(job *batchv1.Job) string {
	raw, _ := json.Marshal(job.Spec.Template) // a PodTemplateSpec always marshals
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

func stampScriptHashes(jobs []batchv1.Job) {
	for i := range jobs {
		if jobs[i].Annotations == nil {
			jobs[i].Annotations = map[string]string{}
		}
		jobs[i].Annotations[provisioningScriptHashAnnotation] = scriptHash(&jobs[i])
	}
}

// retireStaleProvisioningJobs deletes finished provisioning Jobs whose script no
// longer matches the desired one, so they are re-created and re-run. It is how
// a finished Job runs again: none of them carries a TTL (keepFinishedJobs).
//
// Only finished Jobs are touched: a running one is either about to converge
// anyway or is being watched by a reconcile that expects it to exist. Jobs
// predating this annotation are treated as stale, which re-runs them once —
// safe, because every provisioning script here is written to be re-runnable.
func (r *TenantReconciler) retireStaleProvisioningJobs(ctx context.Context, jobs []batchv1.Job) {
	for i := range jobs {
		desired := jobs[i].Annotations[provisioningScriptHashAnnotation]
		live := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: jobs[i].Name, Namespace: jobs[i].Namespace,
		}, live); err != nil {
			continue
		}
		if live.Annotations[provisioningScriptHashAnnotation] == desired {
			continue
		}
		if live.Status.Succeeded == 0 && live.Status.Failed == 0 {
			continue
		}
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, live, &client.DeleteOptions{PropagationPolicy: &prop})
	}
}
