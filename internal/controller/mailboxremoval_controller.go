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
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// What becomes of a removed person's mailbox.
//
// Whoever removes a person from a tenant that keeps mailboxes on the
// cluster's own mail server says, at that moment, whether the mailbox is
// archived or deleted. The registrar takes the answer and removes the
// person; it has no access to the mail server's volume or namespace and is
// given none. It writes the answer down as a MailboxRemoval, and this
// reconciler carries it out, by a Job beside the mail server's volume
// (backup/mailbox_person.go), and reports on the record what happened.
//
// The record is written by a process that takes requests from people, and
// this one holds the rights that destroy mail. So nothing in a record is
// acted on because it says so:
//
//   - the address must be in the mail domain the tenant has, and its first
//     half a plain name -- or the record is refused;
//   - nobody of any realm that domain's addresses live in may hold the
//     address, switched off or not -- a mailbox that still has a person is
//     never touched, whatever asked;
//   - no mail password of the address may be left with the mail server, and
//     the server is given time to have read that, before the directory is
//     moved or removed.
//
// A refusal changes nothing and is final for that record. A Job that fails
// is said on the record and run again, less and less often.

const (
	// mailboxPersonGrace is how long a record waits for the person to be
	// gone from the realm. The registrar writes the record first and removes
	// the person next; a record whose person is still there after this was
	// never followed by a removal, and is refused.
	mailboxPersonGrace = 2 * time.Minute
	// mailboxSignInSettle is how long after the last mail password of the
	// address is out of the mail server's files the mailbox is left alone:
	// the files reach the server through a mounted Secret, which the node
	// refreshes within about a minute.
	mailboxSignInSettle = 2 * time.Minute
	// mailboxRecordKept is how long the record of a mailbox that is gone --
	// deleted, never there, or refused -- stays to be read. It names a
	// person's address, and nothing needs it after that. The record of an
	// archived mailbox stays for as long as the archive does.
	mailboxRecordKept = 30 * 24 * time.Hour
	mailboxPoll       = 5 * time.Second
	mailboxRetryMax   = 30 * time.Minute
)

// MailboxRemovalReconciler carries out MailboxRemoval records.
type MailboxRemovalReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes. Nil holds nothing.
	Definitions *crdcheck.Holder
	client.Client
	// Tenant supplies the mail reconciler's own answers: the tenant's mail
	// domain, the mail server's volume, the mail passwords, the realm's
	// people.
	Tenant *TenantReconciler
	// Holders lists the addresses the people of a realm hold. Nil asks the
	// identity provider.
	Holders func(ctx context.Context, realm string) (map[string]bool, error)
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
}

// Create: a restore puts the archived mailboxes of a bundle on record
// (recordArchivedMailboxes). Delete: the record of a mailbox that is gone is
// removed in time, and a tenant deleted with its data takes its records with
// it. Never update: what was decided is the registrar's to write.
// +kubebuilder:rbac:groups=gentianos.io,resources=mailboxremovals,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=mailboxremovals/status,verbs=get;update;patch

func (r *MailboxRemovalReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *MailboxRemovalReconciler) holders(ctx context.Context, realm string) (map[string]bool, error) {
	if r.Holders != nil {
		return r.Holders(ctx, realm)
	}
	return r.Tenant.keycloakRealmHolders(ctx, realm)
}

// Reconcile carries one record a step further.
func (r *MailboxRemovalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	record := &gentianov1alpha1.MailboxRemoval{}
	if err := r.Get(ctx, req.NamespacedName, record); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if record.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	if mailboxRemovalSettled(record) {
		if record.Status.Phase == gentianov1alpha1.MailboxRemovalArchived {
			return ctrl.Result{}, nil
		}
		since := record.CreationTimestamp.Time
		if record.Status.DeletedAt != nil {
			since = record.Status.DeletedAt.Time
		}
		if left := mailboxRecordKept - r.now().Sub(since); left > 0 {
			return ctrl.Result{RequeueAfter: left}, nil
		}
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, record))
	}
	before := record.Status.DeepCopy()
	result, err := r.step(ctx, record)
	if err != nil {
		// Said on the record as well as returned: the console reads the
		// record, and "pending" with no reason is not an answer.
		if record.Status.Phase == "" {
			record.Status.Phase = gentianov1alpha1.MailboxRemovalPending
		}
		record.Status.Message = "Not done yet: " + err.Error()
	}
	if !mailboxStatusEqual(before, &record.Status) {
		if updateErr := r.Status().Update(ctx, record); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
	}
	return result, err
}

// mailboxRemovalSettled reports whether nothing is left to do for a record.
func mailboxRemovalSettled(record *gentianov1alpha1.MailboxRemoval) bool {
	if record.Status.Refused {
		return true
	}
	switch record.Status.Phase {
	case gentianov1alpha1.MailboxRemovalDeleted, gentianov1alpha1.MailboxRemovalNoMailbox:
		return true
	case gentianov1alpha1.MailboxRemovalArchived:
		return record.Spec.DeleteArchive == nil
	}
	return false
}

func mailboxStatusEqual(a, b *gentianov1alpha1.MailboxRemovalStatus) bool {
	same := func(x, y *metav1.Time) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Equal(y)
	}
	return a.Phase == b.Phase && a.Message == b.Message && a.Refused == b.Refused && a.Domain == b.Domain &&
		a.Archive == b.Archive && a.SizeBytes == b.SizeBytes && a.Messages == b.Messages && a.Attempts == b.Attempts &&
		same(a.ArchivedAt, b.ArchivedAt) && same(a.DeletedAt, b.DeletedAt) && same(a.SignInGoneAt, b.SignInGoneAt) && same(a.LastFailureAt, b.LastFailureAt)
}

func (r *MailboxRemovalReconciler) refuse(record *gentianov1alpha1.MailboxRemoval, format string, args ...any) (ctrl.Result, error) {
	record.Status.Phase = gentianov1alpha1.MailboxRemovalFailed
	record.Status.Refused = true
	record.Status.Message = "Refused: " + fmt.Sprintf(format, args...) + ". Nothing was changed."
	return ctrl.Result{}, nil
}

func (r *MailboxRemovalReconciler) wait(record *gentianov1alpha1.MailboxRemoval, after time.Duration, format string, args ...any) (ctrl.Result, error) {
	if record.Status.Phase != gentianov1alpha1.MailboxRemovalArchived {
		record.Status.Phase = gentianov1alpha1.MailboxRemovalPending
	}
	record.Status.Message = fmt.Sprintf(format, args...)
	return ctrl.Result{RequeueAfter: after}, nil
}

func (r *MailboxRemovalReconciler) step(ctx context.Context, record *gentianov1alpha1.MailboxRemoval) (ctrl.Result, error) {
	// A record a restore wrote for an archived mailbox it put back: there is
	// no mailbox to move, and no Job is ever run on the strength of it. What
	// it states is reported, once.
	if restored := record.Spec.Restored; restored != nil && record.Status.Phase != gentianov1alpha1.MailboxRemovalArchived {
		if backup.ValidArchiveName(restored.Archive) != nil || backup.ValidMailDomain(restored.Domain) != nil {
			return r.refuse(record, "the record of a restored archive names %q in %q, which is not an archived mailbox", restored.Archive, restored.Domain)
		}
		record.Status.Phase = gentianov1alpha1.MailboxRemovalArchived
		record.Status.Archive, record.Status.Domain = restored.Archive, restored.Domain
		record.Status.ArchivedAt = restored.ArchivedAt
		record.Status.SizeBytes, record.Status.Messages = restored.SizeBytes, restored.Messages
		record.Status.Message = "Archived, and put back from a backup as an archived mailbox."
		if record.Spec.DeleteArchive == nil {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: record.Spec.Tenant}, tenant); err != nil {
		if errors.IsNotFound(err) {
			// Not a refusal: a retired tenant keeps its mailboxes, and comes
			// back under its name. Until then there is no domain to go by.
			return r.wait(record, mailboxRetryMax, "Waiting: the tenant %s is not on this cluster.", record.Spec.Tenant)
		}
		return ctrl.Result{}, err
	}
	if tenant.DeletionTimestamp != nil {
		return r.wait(record, time.Minute, "Waiting: the tenant %s is being removed, and its removal decides what becomes of its mailboxes.", tenant.Name)
	}
	volume, err := r.Tenant.mailVolume(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The archived mailbox is to be deleted: it is where the record says it
	// was put, whatever the tenant's domain is now.
	if record.Status.Phase == gentianov1alpha1.MailboxRemovalArchived {
		if volume == nil {
			return r.wait(record, mailboxRetryMax, "Waiting: this cluster's mail server and its volume are not there.")
		}
		job, err := backup.ArchivedMailboxDeleteJob(backup.MailboxTarget{
			Namespace: mailNamespace, Claim: volume.Claim, Node: volume.Node, Tenant: tenant.Name, Record: record.Name,
			Domain: record.Status.Domain, Archive: record.Status.Archive,
		})
		if err != nil {
			// Still archived, and said as that: the archive is there, and a
			// record that stopped saying so would hide it.
			record.Status.Message = fmt.Sprintf("The archived mailbox was not deleted: %v.", err)
			return ctrl.Result{}, nil
		}
		return r.run(ctx, record, job)
	}

	domain := r.Tenant.mailboxDomainOf(ctx, tenant)
	if domain == "" || volume == nil {
		record.Status.Phase = gentianov1alpha1.MailboxRemovalNoMailbox
		record.Status.Message = "The tenant's people have no mailboxes on this cluster's own mail server."
		return ctrl.Result{}, nil
	}
	name, at, ok := cutAddress(record.Spec.Address)
	if !ok || !strings.EqualFold(at, domain) {
		return r.refuse(record, "%s is not an address in %s, the domain tenant %s has its mailboxes under", record.Spec.Address, domain, tenant.Name)
	}
	name = strings.ToLower(name)
	if err := backup.ValidMailboxName(name); err != nil {
		return r.refuse(record, "%v", err)
	}
	address := name + "@" + domain

	// One decision per mailbox: the last one made. An earlier one that was
	// not carried out -- its person was not removed then -- must not come
	// alive when the person is removed later with another answer.
	if later, err := r.laterDecision(ctx, record, address); err != nil {
		return ctrl.Result{}, err
	} else if later != "" {
		return r.refuse(record, "a later decision about %s replaces this one (%s)", address, later)
	}

	// Nobody holds the address.
	realms, err := r.Tenant.realmsOfMailDomain(ctx, domain)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, realm := range realms {
		held, err := r.holders(ctx, realm)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("the people of realm %s could not be listed, and a mailbox is not touched while it may be somebody's: %w", realm, err)
		}
		if !held[address] {
			continue
		}
		if r.now().Sub(record.CreationTimestamp.Time) < mailboxPersonGrace {
			return r.wait(record, mailboxPoll, "Waiting: %s still belongs to a person of realm %s.", address, realm)
		}
		return r.refuse(record, "%s belongs to a person of realm %s, and a mailbox that has a person is not archived or deleted", address, realm)
	}

	// Nobody can sign in to it. The tenant's mail passwords are derived
	// from its realm's people now, not at the tenant's next turn.
	if err := r.Tenant.syncMailAppPasswords(ctx, tenant); err != nil && !r.Tenant.adoptsKernelRealm(tenant) {
		return ctrl.Result{}, fmt.Errorf("the mail passwords of tenant %s could not be written again without the person's: %w", tenant.Name, err)
	}
	// And under a recipient list the address stops receiving now.
	if err := r.Tenant.syncPostfixVirtualMailboxMaps(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("the mail server's list of recipients could not be written again: %w", err)
	}
	listed, err := r.Tenant.mailSignInListed(ctx, address)
	if err != nil {
		return ctrl.Result{}, err
	}
	if listed {
		record.Status.SignInGoneAt = nil
		return r.wait(record, mailboxPoll, "Waiting: a mail password of %s is still with the mail server.", address)
	}
	if record.Status.SignInGoneAt == nil {
		now := metav1.NewTime(r.now())
		record.Status.SignInGoneAt = &now
	}
	if left := mailboxSignInSettle - r.now().Sub(record.Status.SignInGoneAt.Time); left > 0 {
		return r.wait(record, left, "Waiting until the mail server has read that nobody signs in as %s any more.", address)
	}

	target := backup.MailboxTarget{
		Namespace: mailNamespace, Claim: volume.Claim, Node: volume.Node, Tenant: tenant.Name, Record: record.Name,
		Domain: domain, Mailbox: name, Archive: backup.ArchiveName(name, record.CreationTimestamp.Time),
	}
	var job *batchv1.Job
	switch record.Spec.Mailbox {
	case gentianov1alpha1.MailboxChoiceArchive:
		job, err = backup.MailboxArchiveJob(target)
	case gentianov1alpha1.MailboxChoiceDelete:
		job, err = backup.MailboxDeleteJob(target)
	default:
		// The schema admits two words. Anything else is not a choice, and
		// the safe reading of no choice is to do nothing.
		return r.refuse(record, "%q is neither archive nor delete", record.Spec.Mailbox)
	}
	if err != nil {
		return r.refuse(record, "%v", err)
	}
	record.Status.Domain = domain
	return r.run(ctx, record, job)
}

// laterDecision names a record made after this one for the same tenant and
// address, "" when this is the last.
func (r *MailboxRemovalReconciler) laterDecision(ctx context.Context, record *gentianov1alpha1.MailboxRemoval, address string) (string, error) {
	all := &gentianov1alpha1.MailboxRemovalList{}
	if err := r.List(ctx, all); err != nil {
		return "", err
	}
	for i := range all.Items {
		other := &all.Items[i]
		if other.Name == record.Name || other.Spec.Tenant != record.Spec.Tenant || !strings.EqualFold(other.Spec.Address, address) {
			continue
		}
		mine, theirs := record.CreationTimestamp.Time, other.CreationTimestamp.Time
		if theirs.After(mine) || (theirs.Equal(mine) && other.Name > record.Name) {
			return other.Name, nil
		}
	}
	return "", nil
}

func cutAddress(address string) (name, domain string, ok bool) {
	at := strings.LastIndex(address, "@")
	if at <= 0 || at == len(address)-1 {
		return "", "", false
	}
	return address[:at], address[at+1:], true
}

// run makes sure the Job exists, and reads what it did once it has ended.
func (r *MailboxRemovalReconciler) run(ctx context.Context, record *gentianov1alpha1.MailboxRemoval, want *batchv1.Job) (ctrl.Result, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, job)
	if errors.IsNotFound(err) {
		if left := r.backoff(record); left > 0 {
			return ctrl.Result{RequeueAfter: left}, nil
		}
		if err := r.Create(ctx, want); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("start the Job %s/%s: %w", want.Namespace, want.Name, err)
		}
		if record.Status.Attempts == 0 {
			return r.wait(record, mailboxPoll, "The mail server's volume is being worked on (Job %s/%s).", want.Namespace, want.Name)
		}
		// After a failure the record goes on saying what failed.
		return ctrl.Result{RequeueAfter: mailboxPoll}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if job.Annotations[backup.MailboxRecordAnnotation] != record.Name {
		return ctrl.Result{}, fmt.Errorf("the Job %s/%s is not this record's", job.Namespace, job.Name)
	}
	switch {
	case jobIsComplete(job):
		outcome, err := r.outcome(ctx, job)
		if err != nil {
			// A Job that ended well and left no word is run again: every
			// script here says the same thing the second time.
			if delErr := r.deleteJob(ctx, job); delErr != nil {
				return ctrl.Result{}, delErr
			}
			return ctrl.Result{RequeueAfter: mailboxPoll}, nil
		}
		r.report(record, outcome)
		return ctrl.Result{}, nil
	case jobIsFailed(job):
		// A failed Job is counted once. It is deleted then, and can still be
		// read for a moment afterwards.
		if job.DeletionTimestamp != nil || (record.Status.LastFailureAt != nil && !job.CreationTimestamp.After(record.Status.LastFailureAt.Time)) {
			if err := r.deleteJob(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: mailboxPoll}, nil
		}
		reason := "the Job did not succeed"
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				reason = strings.TrimSpace(c.Reason + " " + c.Message)
			}
		}
		if said := r.lastWords(ctx, job); said != "" {
			reason += ": " + said
		}
		now := metav1.NewTime(r.now())
		record.Status.Attempts++
		record.Status.LastFailureAt = &now
		if record.Status.Phase != gentianov1alpha1.MailboxRemovalArchived {
			record.Status.Phase = gentianov1alpha1.MailboxRemovalFailed
		}
		record.Status.Message = fmt.Sprintf("Failed, and tried again in %s (attempt %d): %s", r.backoffFor(record.Status.Attempts).Round(time.Second), record.Status.Attempts, reason)
		log.FromContext(ctx).Error(nil, "a removed person's mailbox could not be worked on", "record", record.Name, "tenant", record.Spec.Tenant, "job", job.Name, "reason", reason)
		if err := r.deleteJob(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.backoffFor(record.Status.Attempts)}, nil
	}
	return ctrl.Result{RequeueAfter: mailboxPoll}, nil
}

func (r *MailboxRemovalReconciler) backoffFor(attempts int32) time.Duration {
	wait := time.Minute
	for i := int32(1); i < attempts && wait < mailboxRetryMax; i++ {
		wait *= 2
	}
	if wait > mailboxRetryMax {
		wait = mailboxRetryMax
	}
	return wait
}

// backoff is how long is left before the next attempt may start.
func (r *MailboxRemovalReconciler) backoff(record *gentianov1alpha1.MailboxRemoval) time.Duration {
	if record.Status.Attempts == 0 || record.Status.LastFailureAt == nil {
		return 0
	}
	return r.backoffFor(record.Status.Attempts) - r.now().Sub(record.Status.LastFailureAt.Time)
}

func (r *MailboxRemovalReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
	policy := metav1.DeletePropagationBackground
	return client.IgnoreNotFound(r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}))
}

func (r *MailboxRemovalReconciler) pods(ctx context.Context, job *batchv1.Job) []corev1.Pod {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return nil
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	return pods.Items
}

// outcome reads what the Job's pod that succeeded said it did.
func (r *MailboxRemovalReconciler) outcome(ctx context.Context, job *batchv1.Job) (backup.MailboxOutcome, error) {
	for _, pod := range r.pods(ctx, job) {
		if pod.Status.Phase != corev1.PodSucceeded {
			continue
		}
		for _, c := range pod.Status.ContainerStatuses {
			if c.State.Terminated != nil && c.State.Terminated.ExitCode == 0 {
				return backup.ParseMailboxOutcome(c.State.Terminated.Message)
			}
		}
	}
	return backup.MailboxOutcome{}, fmt.Errorf("the Job %s/%s succeeded and its pod is not there to say what it did", job.Namespace, job.Name)
}

// lastWords is what a failed Job's newest pod was last known to say.
func (r *MailboxRemovalReconciler) lastWords(ctx context.Context, job *batchv1.Job) string {
	for _, pod := range r.pods(ctx, job) {
		for _, c := range pod.Status.ContainerStatuses {
			if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
				return strings.TrimSpace(fmt.Sprintf("exit code %d %s %s", t.ExitCode, t.Reason, t.Message))
			}
			if w := c.State.Waiting; w != nil && w.Reason != "" {
				return strings.TrimSpace(w.Reason + " " + w.Message)
			}
		}
		if pod.Status.Phase == corev1.PodPending {
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
					return strings.TrimSpace(c.Reason + " " + c.Message)
				}
			}
		}
	}
	return ""
}

func (r *MailboxRemovalReconciler) report(record *gentianov1alpha1.MailboxRemoval, outcome backup.MailboxOutcome) {
	now := metav1.NewTime(r.now())
	status := &record.Status
	wasArchived := status.Phase == gentianov1alpha1.MailboxRemovalArchived
	switch outcome.Result {
	case backup.MailboxArchived:
		status.Phase = gentianov1alpha1.MailboxRemovalArchived
		name, _, _ := cutAddress(record.Spec.Address)
		status.Archive = backup.ArchiveName(strings.ToLower(name), record.CreationTimestamp.Time)
		status.ArchivedAt = &now
		status.SizeBytes, status.Messages = outcome.Bytes, outcome.Messages
		status.Message = fmt.Sprintf("Archived: %d message(s). Nobody receives or signs in at the address; the mail is kept until the archived mailbox is deleted, or the tenant is.", outcome.Messages)
	case backup.MailboxDeleted:
		status.Phase = gentianov1alpha1.MailboxRemovalDeleted
		status.DeletedAt = &now
		if wasArchived {
			status.Message = "The archived mailbox was deleted."
		} else {
			status.SizeBytes, status.Messages = outcome.Bytes, outcome.Messages
			status.Message = fmt.Sprintf("Deleted: %d message(s).", outcome.Messages)
		}
	case backup.MailboxAbsent:
		if wasArchived {
			// It was archived, and is not there to delete: gone all the same.
			status.Phase = gentianov1alpha1.MailboxRemovalDeleted
			status.DeletedAt = &now
			status.Message = "The archived mailbox was not there any more."
			return
		}
		status.Phase = gentianov1alpha1.MailboxRemovalNoMailbox
		status.Message = "There was no mailbox: nothing was ever delivered to the address."
	}
}

// SetupWithManager registers the reconciler.
func (r *MailboxRemovalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// A record's own status changing asks for nothing: what is waited
		// for is asked for again by time.
		For(&gentianov1alpha1.MailboxRemoval{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("mailboxremoval").
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.MailboxRemoval{}, r))
}
