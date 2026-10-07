/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Purging an app: destroying what uninstalling it kept.
//
// Uninstalling takes an app's workloads and its sign-in client away and keeps
// everything it stored. A purge is the other act, asked for separately, and
// it is the one that cannot be undone. Two rules shape everything below.
//
// It is one request, answered when it is over. Nothing here continues in the
// background and nothing reconciles towards "purged": a person asked for data
// to be destroyed and is told, in the answer to that request, what was.
//
// It fails loudly. A step that could not do its work ends the purge there and
// the answer names the step, what had been destroyed before it and what was
// not attempted. Nothing is skipped because a pod, a token or a profile could
// not be found, and no command's failure is discarded. Every step can be
// repeated, so the remedy for a purge that did not complete is to ask again.

// The kinds of data an app can leave behind, as the shared inventory names
// them (backup.AppKinds). The purge reports in these words, and so does the
// read of what uninstalled apps still hold (retained.go), so that one can be
// matched against the other.
const (
	KindDatabase      = string(backup.KindDatabase)
	KindObjectStorage = string(backup.KindObjectStorage)
	KindCache         = string(backup.KindCache)
	KindFiles         = string(backup.KindFiles)
	KindCredentials   = string(backup.KindCredentials)
	KindAccessGroup   = string(backup.KindAccessGroup)
	KindRecords       = string(backup.KindRecords)
)

// kindLabels are the kinds as a person reads them.
var kindLabels = map[string]string{
	KindDatabase:      "database",
	KindObjectStorage: "object storage",
	KindCache:         "cache",
	KindFiles:         "files",
	KindCredentials:   "stored credentials",
	KindAccessGroup:   "access group",
	KindRecords:       "provisioning records",
}

// The bounds of one purge.
//
// purgeBudget is all the time a purge has, from the moment it holds the app's
// lock. It is the number its caller's deadline is set against: the director
// waits lifecycle.PurgeDeadline for this action, which is longer, so a purge
// is never cut off by the request that asked for it -- it either finishes or
// gives up by itself and says where. The waits below are each shorter than
// the budget and several can follow one another, so the budget is what ends a
// purge that is going badly; a wait that hits it reports that, and the retry
// starts with every finished step already done.
//
// Variables so that a test does not take minutes; nothing else assigns them.
var (
	purgeBudget = 4*time.Minute + 30*time.Second
	// purgeJobWait bounds one deletion Job, from created to finished.
	purgeJobWait = 2 * time.Minute
	// pvcDeletionTimeout bounds the wait for deleted volume claims to be
	// gone. A purge must not return while they are still Terminating: the
	// next install of the app would not schedule.
	pvcDeletionTimeout = 2 * time.Minute
	// purgeRecordWait bounds the wait for a deleted record to be gone.
	purgeRecordWait = 30 * time.Second
	purgePoll       = 3 * time.Second
)

// purgeJobDeadlineSeconds ends a deletion Job that hangs before the wait for
// it does, so the Job reports that it failed rather than the purge reporting
// only that it stopped waiting.
const purgeJobDeadlineSeconds = int64(backup.DestroyWithinARequest)

// PurgeError is a purge that did not complete.
type PurgeError struct {
	Tenant  string
	Profile string
	// Step is the kind whose destruction failed.
	Step string
	// Destroyed are the kinds destroyed before it, in order. They stay
	// destroyed: nothing is rolled back.
	Destroyed []string
	// Pending are the kinds that were not attempted.
	Pending []string
	Err     error
}

func (e *PurgeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the purge of %s in %s did not complete: destroying its %s failed: %v.",
		e.Profile, e.Tenant, kindLabels[e.Step], e.Err)
	if len(e.Destroyed) > 0 {
		fmt.Fprintf(&b, " Already destroyed: %s.", labelled(e.Destroyed))
	} else {
		b.WriteString(" Nothing had been destroyed before that.")
	}
	if len(e.Pending) > 0 {
		fmt.Fprintf(&b, " Not attempted: %s.", labelled(e.Pending))
	}
	b.WriteString(" Nothing is rolled back. Retry the purge: every step is safe to repeat, and it continues with what is left.")
	return b.String()
}

func (e *PurgeError) Unwrap() error { return e.Err }

func labelled(kinds []string) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, kindLabels[k])
	}
	return strings.Join(out, ", ")
}

// purgeStep destroys one kind of data. run returns nil only when the kind is
// gone: destroyed now, or found already absent.
type purgeStep struct {
	kind string
	run  func(ctx context.Context) error
}

// purgeSteps is what a purge of this app does, in order.
//
// The order is the shared inventory's teardown order -- provisioning's,
// reversed -- and the same one the deletion of a tenant follows: the files
// first, then the stores, then the access group, the stored credentials and
// last the records, which the purge's own Jobs add to. A kind the app does
// not have is not a step. Which stores it has is declared by its profile and
// by nothing else, which is why a purge is refused without one.
func (s *Service) purgeSteps(tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile, app string) []purgeStep {
	inv := backup.InventoryOf(tenant, app, profile)
	extensions := backup.SidecarNames(profile)
	stores := backup.StoreDestroyJobs(tenant, app, inv.Stores, backup.DestroyWithinARequest)
	how := map[backup.Kind]func(ctx context.Context) error{
		backup.KindFiles: func(ctx context.Context) error {
			return s.purgePVCs(ctx, tenant, app, profile)
		},
		backup.KindCredentials: func(ctx context.Context) error {
			return s.purgeCredentials(ctx, tenant, app, extensions)
		},
		backup.KindAccessGroup: func(ctx context.Context) error {
			return s.purgeAccessGroup(ctx, tenant, app, extensions)
		},
		backup.KindRecords: func(ctx context.Context) error {
			return s.purgeClusterArtifacts(ctx, tenant, app)
		},
	}
	for kind, job := range stores {
		how[kind] = func(ctx context.Context) error { return s.runKernelJob(ctx, job) }
	}
	if inv.DatabaseRecord != "" {
		drop := how[backup.KindDatabase]
		how[backup.KindDatabase] = func(ctx context.Context) error {
			if err := drop(ctx); err != nil {
				return err
			}
			// The record of the database goes only now that the database
			// has: it is what says an uninstalled app still holds one, and
			// removing it first would leave a database nothing reports if
			// the drop failed.
			return s.deleteDatabaseRecord(ctx, tenant.Name, app)
		}
	}
	var steps []purgeStep
	for _, kind := range backup.AppPurgeOrder() {
		if run, ok := how[kind]; ok {
			steps = append(steps, purgeStep{string(kind), run})
		}
	}
	return steps
}

// ErrCannotPurgeNow is a purge that was not begun because something it needs
// in order to finish is not there. Nothing was destroyed.
var ErrCannotPurgeNow = errors.New("the purge cannot be completed now")

// purgePreflight establishes, before anything is destroyed, everything that
// can be known beforehand about whether the purge can finish: the realm the
// app's group is in and that the identity provider answers for it, that the
// vault answers, that the database server has a primary to drop on, and that
// the app's database is of an engine this platform can drop. A purge that
// would have stopped half-way for any of these stops here instead, with
// everything still in place.
func (s *Service) purgePreflight(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s. Nothing was destroyed; ask again once that is put right",
			ErrCannotPurgeNow, fmt.Sprintf(format, args...))
	}
	switch engine := backup.ProfileStores(profile).Database; engine {
	case "", gentianov1alpha1.DatabaseEngineMariaDB:
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		if _, err := s.postgresPrimary(ctx); err != nil {
			return refuse("%v", err)
		}
	default:
		return refuse("this platform cannot drop a %q database", engine)
	}
	if s.vault == nil {
		return refuse("the operator has no connection to the vault (BAO_ADDR is not set), so the app's stored credentials could not be deleted")
	}
	if _, err := s.vault.ListChildren(ctx, secrets.AppsPath(tenant.Name)); err != nil {
		return refuse("the vault does not answer: %v", err)
	}
	realm := keycloak.RealmName(tenant)
	groups, err := s.accessGroups(ctx)
	if err != nil {
		return refuse("the identity provider cannot be asked: %v", err)
	}
	if _, err := groups.GroupNames(ctx, realm, keycloak.TenantAppGroup(tenant.Name, "")); err != nil {
		return refuse("the identity provider does not answer for the tenant's realm %s: %v", realm, err)
	}
	return nil
}

// purge runs the steps in order and stops at the first that fails.
//
// Stopping is deliberate. A step that failed leaves the app in a state nobody
// has looked at, and carrying on would destroy more on the strength of it.
func (s *Service) purge(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile, app string) (destroyed []string, err error) {
	logger := log.FromContext(ctx).WithName("purge").WithValues("tenant", tenant.Name, "app", app)
	steps := s.purgeSteps(tenant, profile, app)
	for i, step := range steps {
		if err := step.run(ctx); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("the purge ran out of the %s it is given (%w)", purgeBudget, err)
			}
			pending := make([]string, 0, len(steps)-i-1)
			for _, later := range steps[i+1:] {
				pending = append(pending, later.kind)
			}
			logger.Error(err, "a purge step failed", "step", step.kind, "destroyed", destroyed, "pending", pending)
			return destroyed, &PurgeError{
				Tenant: tenant.Name, Profile: app, Step: step.kind,
				Destroyed: destroyed, Pending: pending, Err: err,
			}
		}
		logger.Info("purged", "kind", step.kind)
		destroyed = append(destroyed, step.kind)
	}
	return destroyed, nil
}

// wait pauses for one poll interval, or returns why the purge must stop.
func wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(purgePoll):
		return nil
	}
}

// --- database ---------------------------------------------------------------

// deleteDatabaseRecord removes the CloudNativePG Database object the tenant's
// provisioning made for the app, and waits for it to be gone.
func (s *Service) deleteDatabaseRecord(ctx context.Context, tenant, app string) error {
	name := cnpgDatabaseName(tenant, app)
	key := func() *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cnpgDatabaseGVK)
		obj.SetName(name)
		obj.SetNamespace(layout.System("postgresql"))
		return obj
	}
	if err := s.client.Delete(ctx, key()); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the database record %s: %w", name, err)
	}
	deadline := time.Now().Add(purgeRecordWait)
	for {
		obj := key()
		err := s.client.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look for the database record %s: %w", name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the database record %s is still there %s after it was deleted", name, purgeRecordWait)
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
}

var cnpgDatabaseGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"}

// postgresPrimary is the one instance of the shared cluster that accepts
// writes.
//
// The database is dropped by a Job that connects through the cluster's
// read-write Service, which is the primary whichever pod that is; this is
// the check, made before anything is destroyed, that there is one. A purge
// used to exec into whichever pod the API server listed first, and a replica
// refuses DROP DATABASE.
func (s *Service) postgresPrimary(ctx context.Context) (string, error) {
	const selector = "cnpg.io/cluster=postgres,cnpg.io/instanceRole=primary"
	ns := layout.System("postgresql")
	pods, err := s.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("look for the PostgreSQL primary in %s: %w", ns, err)
	}
	var running []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			running = append(running, pod.Name)
		}
	}
	switch len(running) {
	case 1:
		return running[0], nil
	case 0:
		return "", fmt.Errorf("no running PostgreSQL primary in %s (%s); nothing was dropped", ns, selector)
	default:
		return "", fmt.Errorf("%d pods in %s claim to be the PostgreSQL primary (%s); nothing was dropped",
			len(running), ns, strings.Join(running, ", "))
	}
}

// --- deletion Jobs ----------------------------------------------------------

// The Jobs themselves, and the scripts they run, are the shared ones in
// internal/backup/teardown.go: the deletion of a tenant runs the same.

// runKernelJob runs one deletion Job to its end and reports how it ended.
func (s *Service) runKernelJob(ctx context.Context, job *batchv1.Job) error {
	jobs := s.clientset.BatchV1().Jobs(job.Namespace)
	// A Job left by an earlier purge of the same app: it has to be gone
	// before this one can be created under the same name.
	err := jobs.Delete(ctx, job.Name, metav1.DeleteOptions{
		PropagationPolicy: ptr(metav1.DeletePropagationBackground),
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove the previous Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	deadline := time.Now().Add(purgeJobWait)
	for {
		_, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			return fmt.Errorf("look for the previous Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the previous Job %s/%s was still there after %s", job.Namespace, job.Name, purgeJobWait)
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	deadline = time.Now().Add(purgeJobWait)
	for {
		j, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				return nil
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return fmt.Errorf("the Job %s/%s failed (%s %s)%s", job.Namespace, job.Name,
					c.Reason, c.Message, s.jobOutput(ctx, job))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Job %s/%s had not finished after %s%s", job.Namespace, job.Name,
				purgeJobWait, s.jobOutput(ctx, job))
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
}

// jobOutput is the end of what a Job's last pod printed, for the message
// about its failure. Empty when it cannot be read: the failure is reported
// either way, and this only adds the reason to it.
func (s *Service) jobOutput(ctx context.Context, job *batchv1.Job) string {
	pods, err := s.clientset.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job.Name,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	last := pods.Items[0]
	for _, p := range pods.Items[1:] {
		if p.CreationTimestamp.After(last.CreationTimestamp.Time) {
			last = p
		}
	}
	raw, err := s.clientset.CoreV1().Pods(job.Namespace).
		GetLogs(last.Name, &corev1.PodLogOptions{TailLines: ptr(int64(10))}).DoRaw(ctx)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	return "; its output ended: " + strings.Join(strings.Fields(string(raw)), " ")
}

// --- stored credentials -----------------------------------------------------

// CredentialStore is the vault as a purge and the retained-data read use it:
// remove a subtree, and name what is below a path. *secrets.KVClient is one.
type CredentialStore interface {
	DeleteTree(ctx context.Context, logicalPath string) error
	ListChildren(ctx context.Context, logicalPath string) ([]string, error)
}

// ownedKeys is the shared inventory's (backup.OwnedKeys).
func ownedKeys(tenant *gentianov1alpha1.Tenant, app string, extensions []string) []string {
	return backup.OwnedKeys(tenant, app, extensions)
}

// credentialPaths are the vault subtrees one app owns: its own, and one per
// extension. Everything provisioning seeds for the app (its database, bucket,
// cache, mail and sign-in credentials) and every secret generated for it is
// below one of these.
//
// A contract's path (…/contracts/<name>) is not among them: it is shared by
// the app that offers it and every app that consumes it, and belongs to none.
func credentialPaths(tenant *gentianov1alpha1.Tenant, app string, extensions []string) []string {
	var paths []string
	for _, key := range ownedKeys(tenant, app, extensions) {
		paths = append(paths, secrets.AppPath(tenant.Name, key))
	}
	return paths
}

// purgeCredentials deletes every vault path the app owns, all versions and
// their metadata, over the operator's own authenticated session.
//
// This used to exec a script into the vault's pod. The script addressed the
// vault at a name it does not answer on, and every command in it ended in
// `|| true`: it deleted nothing and the purge reported the credentials
// destroyed. A path that cannot be deleted fails the purge
// now, and so does a cluster where the operator has no vault to talk to.
func (s *Service) purgeCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string, extensions []string) error {
	if s.vault == nil {
		return errors.New("the operator has no connection to the vault (BAO_ADDR is not set); no credential was deleted")
	}
	for _, path := range credentialPaths(tenant, app, extensions) {
		if err := s.vault.DeleteTree(ctx, path); err != nil {
			return fmt.Errorf("delete the vault path %s: %w", path, err)
		}
	}
	return nil
}

// --- access group -----------------------------------------------------------

// AccessGroups is the identity provider as a purge and the retained-data read
// use it. *authz.KeycloakAdminClient is one.
type AccessGroups interface {
	DeleteGroup(ctx context.Context, realm, groupName string) (existed bool, err error)
	GroupNames(ctx context.Context, realm, prefix string) ([]string, error)
}

// purgeAccessGroup removes the app's group, and with it everybody's
// membership of it; and the same for each of its extensions, which has a
// group of its own when it signs people in.
//
// The group is what says who may use the app. Uninstalling keeps it, so that
// an app installed again is open to the people it was open to; a purge is the
// end of the app in this tenant, and a group left behind would hand a later
// app of the same name to whoever had the old one.
//
// The realm is the tenant's own (keycloak.RealmName), which need not be
// called what the tenant is; the group's name always carries the tenant's.
func (s *Service) purgeAccessGroup(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string, extensions []string) error {
	groups, err := s.accessGroups(ctx)
	if err != nil {
		return err
	}
	realm := keycloak.RealmName(tenant)
	for _, key := range ownedKeys(tenant, app, extensions) {
		name := keycloak.TenantAppGroup(tenant.Name, key)
		if _, err := groups.DeleteGroup(ctx, realm, name); err != nil {
			return fmt.Errorf("delete the group %s in realm %s: %w", name, realm, err)
		}
	}
	return nil
}

// --- provisioning records ---------------------------------------------------

func (s *Service) purgeClusterArtifacts(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) error {
	tenantNS := tenantNamespace(tenant)
	selector := fmt.Sprintf("%s=%s,gentianos.io/app=%s,%s=%s",
		meta.TenantLabel, tenant.Name, app, meta.ManagedByLabel, meta.ManagedByValue)
	background := metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)}

	// Jobs: the kernel's, and the ones the operator creates directly in the
	// tenant namespace (the app-admins sync among them), which have no owner
	// that Crossplane deletes with the App claim.
	for _, ns := range append(platformNamespaces(), tenantNS) {
		jobs, err := s.clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("list the app's Jobs in %s: %w", ns, err)
		}
		for _, job := range jobs.Items {
			if err := s.clientset.BatchV1().Jobs(ns).Delete(ctx, job.Name, background); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete Job %s/%s: %w", ns, job.Name, err)
			}
		}
	}
	// Pods outlive their Job when whatever deleted the Job did not cascade —
	// Crossplane removing the wrapping Object for a post-install Job orphans its
	// pods exactly this way, so a purged app left completed pods sitting in the
	// namespace with their logs and mounted config. Sweeping by the app selector
	// catches them whatever orphaned them; live pods of the app itself are gone
	// with the Helm release before a purge is admitted.
	tenantPods, err := s.clientset.CoreV1().Pods(tenantNS).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list the app's pods: %w", err)
	}
	for _, pod := range tenantPods.Items {
		if err := s.clientset.CoreV1().Pods(tenantNS).
			Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod.Name, err)
		}
	}
	// Secrets: the kernel's, and the ones the operator writes into the
	// tenant's own namespace — the LLM credentials among them.
	//
	// The selector is app-scoped, so tenant-wide Secrets such as
	// gentian-trust-anchor-tls (labelled managed-by and tenant, but no app) are not
	// matched. Secrets owned by an ExternalSecret are already removed with it when
	// Crossplane deletes the App claim.
	for _, ns := range append(platformNamespaces(), tenantNS) {
		list, err := s.clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("list the app's Secrets in %s: %w", ns, err)
		}
		for _, sec := range list.Items {
			if err := s.clientset.CoreV1().Secrets(ns).Delete(ctx, sec.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete Secret %s/%s: %w", ns, sec.Name, err)
			}
		}
	}
	return nil
}

// --- files ------------------------------------------------------------------

// appVolumes are the claims in the tenant's namespace that are this app's:
// what a purge deletes, what the retained-data read reports and what an
// export copies. The rule is the shared inventory's (backup.AppVolumes).
func appVolumes(claims []corev1.PersistentVolumeClaim, tenant *gentianov1alpha1.Tenant, app string, profile *gentianov1alpha1.ComponentProfile) (own []string, vetoed map[string]string) {
	return backup.AppVolumes(claims, tenant, app, profile)
}

func ownsRelease(tenant *gentianov1alpha1.Tenant, app string, profile *gentianov1alpha1.ComponentProfile, release string) bool {
	return backup.OwnsRelease(tenant, app, profile, release)
}

func (s *Service) purgePVCs(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.ComponentProfile) error {
	ns := tenantNamespace(tenant)
	pvcs, err := s.clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list the volume claims in %s: %w", ns, err)
	}
	doomed, vetoed := appVolumes(pvcs.Items, tenant, appName, profile)
	logger := log.FromContext(ctx).WithName("purge").WithValues("app", appName)
	for name, rel := range vetoed {
		// Not a failure: leaving it is the correct outcome.
		logger.Info("leaving a volume claim that is on record as another Helm release's", "pvc", name, "release", rel)
	}
	if len(doomed) == 0 {
		return nil
	}

	// Delete the pods still holding these volumes first.
	//
	// A PVC carries the pvc-protection finalizer while any pod references it, and
	// that includes pods which have already Succeeded — a finished install Job
	// keeps the claim alive indefinitely. Deleting the PVC alone leaves it
	// Terminating forever, and worse, a reinstall then cannot schedule ("claim is
	// being deleted") while its own Pending pods add fresh references. That is a
	// deadlock that never resolves on its own.
	if err := s.releasePVCHolders(ctx, ns, doomed); err != nil {
		return err
	}
	for _, name := range doomed {
		err := s.clientset.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete volume claim %s/%s: %w", ns, name, err)
		}
	}

	// Wait for them to actually go. A purge that returns while volumes are still
	// Terminating reports success for work it has not finished, and the caller has
	// no way to know the next install will be blocked by it.
	return s.waitForPVCsGone(ctx, ns, doomed)
}

// pvcBelongsToApp reports whether a PVC was provisioned for this app.
func pvcBelongsToApp(pvc corev1.PersistentVolumeClaim, appName, family string) bool {
	return backup.PVCBelongsToApp(pvc, appName, family)
}

// releasePVCHolders deletes pods referencing any of the named claims so the
// pvc-protection finalizer can clear.
func (s *Service) releasePVCHolders(ctx context.Context, ns string, claims []string) error {
	doomed := make(map[string]struct{}, len(claims))
	for _, c := range claims {
		doomed[c] = struct{}{}
	}

	pods, err := s.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list the pods holding volume claims in %s: %w", ns, err)
	}
	for _, pod := range pods.Items {
		if !podReferencesAny(pod, doomed) {
			continue
		}
		err := s.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s/%s, which holds a volume claim being purged: %w", ns, pod.Name, err)
		}
	}
	return nil
}

func podReferencesAny(pod corev1.Pod, claims map[string]struct{}) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		if _, ok := claims[v.PersistentVolumeClaim.ClaimName]; ok {
			return true
		}
	}
	return false
}

// waitForPVCsGone blocks until the claims disappear, or reports which remain.
func (s *Service) waitForPVCsGone(ctx context.Context, ns string, claims []string) error {
	deadline := time.Now().Add(pvcDeletionTimeout)
	for {
		var remaining []string
		for _, name := range claims {
			_, err := s.clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("look for volume claim %s/%s: %w", ns, name, err)
			}
			remaining = append(remaining, name)
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"volume claims still present %s after they were deleted: %s — the next install of this app will not schedule until they are gone",
				pvcDeletionTimeout, strings.Join(remaining, ", "))
		}
		if err := wait(ctx); err != nil {
			return fmt.Errorf("waiting for volume claims %s to be gone: %w", strings.Join(remaining, ", "), err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// platformNamespaces are where tenant provisioning leaves objects: one per
// function, as internal/controller addresses them.
func platformNamespaces() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, ns := range []string{
		layout.Namespace(layout.Authentication),
		layout.Namespace(layout.Provisioning),
		layout.System("postgresql"),
		layout.System("mariadb"),
		layout.System("s3"),
		layout.System("cache"),
		layout.System("mail"),
	} {
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}
