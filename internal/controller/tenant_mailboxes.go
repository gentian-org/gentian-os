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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// Whose mailboxes are a tenant's, and where they are.
//
// One answer for the three acts that need it -- an export copies them, a
// restore puts them back, the tenant's deletion destroys them -- so that
// what a bundle holds is what a deletion takes away.
//
// A tenant's mailboxes are the ones of its mail domain, on the volume of
// the mail server the cluster runs. On a cluster that runs none there are
// no mailboxes, and no act has anything to do.
//
// A mail domain is not always one tenant's. On a single-tenancy cluster the
// user tenant's addresses are on the cluster's own domain, with the
// cluster's administrators' and the addresses the cluster itself receives
// at (multi-tenancy.md §3): one directory, more than one owner, and nothing
// in a mailbox says whose it is. Destroying it with the tenant would destroy
// the cluster's mail, and copying it would put the cluster's mail into a
// tenant's bundle. Such a domain's mailboxes are neither copied nor
// destroyed, and each act says so.
//
// Nothing here reads the mail server's or the relay's own records of which
// domain is whose: the domain is computed as the mail reconciler computes
// it, from the tenant.

// tenantMailboxes is where one tenant's mailboxes are.
type tenantMailboxes struct {
	// Domain is the tenant's mail domain: the directory its mailboxes are
	// below.
	Domain string
	// Claim is the mail server's volume, in the mail namespace.
	Claim string
	// Node is the node the mail server holds the volume on, "" when nothing
	// holds it.
	Node string
}

// mailServerName is the mail server's Deployment, which its chart names
// after the stage; mailVolumeClaim is the claim of its volume, named after
// the server.
func mailServerName() string {
	return fmt.Sprintf("dovecot-%s", envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev")))
}

func mailVolumeClaim() string { return mailServerName() + "-mail" }

// mailboxesOf answers where a tenant's mailboxes are.
//
// nil and no reason: the cluster keeps no mailboxes -- it runs no mail
// server of its own, or is to run one and has none deployed yet. nil and a
// reason: it does, and this tenant's are not its alone to copy or destroy.
// An error: the cluster runs a mail server and its volume could not be
// found, which no act may read as "no mail".
func (r *TenantReconciler) mailboxesOf(ctx context.Context, tenant *gentianov1alpha1.Tenant) (*tenantMailboxes, string, error) {
	if !r.dovecotDeployed(ctx) {
		return nil, "", nil
	}
	present, err := r.mailFunctionPresent(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("look for the mail namespace %s: %w", mailNamespace, err)
	}
	if !present {
		// The cluster is to run its own mail server and does not yet: no
		// mailbox was ever written.
		return nil, "", nil
	}
	domain := mailDomain(tenant, r.KernelDomain, r.TenancyMode)
	if domain == r.KernelDomain {
		return nil, fmt.Sprintf("the mailboxes of %s: the domain is the cluster's own, which this tenant shares with the cluster's administrators and its own addresses, "+
			"and nothing in a mailbox says whose it is", domain), nil
	}
	if err := backup.ValidMailDomain(domain); err != nil {
		return nil, "", fmt.Errorf("the mail domain of tenant %s: %w", tenant.Name, err)
	}
	claim := mailVolumeClaim()
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.Get(ctx, types.NamespacedName{Name: claim, Namespace: mailNamespace}, &corev1.PersistentVolumeClaim{}); err != nil {
		if !errors.IsNotFound(err) {
			return nil, "", fmt.Errorf("look for the mail server's volume %s/%s: %w", mailNamespace, claim, err)
		}
		// No volume. Where the mail server is not there either, the cluster
		// is to run one and none was ever deployed: no mailbox was written.
		// Where the server is there and the volume is not, the mailboxes are
		// somewhere this cannot see, and that is not "no mail".
		server := mailServerName()
		if err := reader.Get(ctx, types.NamespacedName{Name: server, Namespace: mailNamespace}, &appsv1.Deployment{}); err != nil {
			if errors.IsNotFound(err) {
				return nil, "", nil
			}
			return nil, "", fmt.Errorf("look for the mail server %s/%s: %w", mailNamespace, server, err)
		}
		return nil, "", fmt.Errorf("this cluster runs its own mail server (%s/%s), and the volume its mailboxes are on (%s) is not there", mailNamespace, server, claim)
	}
	return &tenantMailboxes{Domain: domain, Claim: claim, Node: nodeHoldingClaim(ctx, r.Client, mailNamespace, claim)}, "", nil
}

// deleteMailboxes destroys the tenant's mailboxes, with deletionPolicy
// Delete. It runs once the tenant's mail routing is gone (deleteMail), so
// that nothing is delivered into a mailbox while it is being destroyed, and
// answers errDeleteJobPending until the Job has succeeded: a Job that fails
// stops the deletion here, as a store's does.
func (r *TenantReconciler) deleteMailboxes(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	boxes, shared, err := r.mailboxesOf(ctx, tenant)
	if err != nil {
		return fmt.Errorf("the mailboxes of tenant %s: %w", tenant.Name, err)
	}
	if boxes == nil {
		if shared != "" {
			log.FromContext(ctx).Info("the tenant's mailboxes are left: they are not the tenant's alone", "tenant", tenant.Name, "why", shared)
		}
		return nil
	}
	return r.ensureDeleteJobs(ctx, mailNamespace, tenant, []string{""},
		func(tenantName, _ string) string { return backup.MailboxDestroyJobName(tenantName) },
		func(t *gentianov1alpha1.Tenant, _ string) *batchv1.Job {
			return backup.MailboxDestroyJob(mailNamespace, t.Name, boxes.Claim, boxes.Node, boxes.Domain, backup.DestroyInTheBackground)
		})
}

// deleteKernelDesktop empties the desktop's database of a tenant that keeps
// it on the kernel's PostgreSQL, with deletionPolicy Delete. Every other
// tenant's is on the tenants' PostgreSQL and is dropped with its apps'
// databases (deleteDatabase).
func (r *TenantReconciler) deleteKernelDesktop(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete || !r.adoptsKernelRealm(tenant) {
		return nil
	}
	return r.ensureDeleteJobs(ctx, backup.KernelPostgresNamespace(), tenant, []string{""},
		func(tenantName, _ string) string { return backup.KernelDesktopDestroyJobName(tenantName) },
		func(t *gentianov1alpha1.Tenant, _ string) *batchv1.Job {
			return backup.KernelDesktopDestroyJob(t.Name, backup.DestroyInTheBackground)
		})
}
