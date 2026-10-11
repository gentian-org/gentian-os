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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// What becomes of a removed person's mailbox, from the record the registrar
// writes to the Job beside the mail server's volume and back.

// realmPeople is an identity provider with people in realms.
type realmPeople struct {
	mu     sync.Mutex
	realms map[string][]keycloakRealmUser
	down   bool
}

func (p *realmPeople) set(realm string, users ...keycloakRealmUser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.realms[realm] = users
}

func (p *realmPeople) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t"})
		case strings.HasSuffix(r.URL.Path, "/users"):
			if p.down {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			parts := strings.Split(r.URL.Path, "/")
			realm := parts[len(parts)-2]
			users := p.realms[realm]
			if r.URL.Query().Get("first") != "0" || users == nil {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_ = json.NewEncoder(w).Encode(users)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type mailboxWorld struct {
	t      *testing.T
	c      client.Client
	tr     *TenantReconciler
	r      *MailboxRemovalReconciler
	people *realmPeople
	now    time.Time
}

var mailboxT0 = time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC)

func mailboxRecord(name, tenant, address string, choice gentianov1alpha1.MailboxChoice) *gentianov1alpha1.MailboxRemoval {
	return &gentianov1alpha1.MailboxRemoval{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(mailboxT0)},
		Spec: gentianov1alpha1.MailboxRemovalSpec{Tenant: tenant, Address: address, Mailbox: choice,
			RequestedBy: gentianov1alpha1.MailboxRequester{Subject: "sub-admin", Name: "admin@demo.k.example"}},
	}
}

func selfhosted(name string) *gentianov1alpha1.Tenant {
	t := planTenant(name)
	t.Spec.Mail = &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeSelfhosted}
	return t
}

func newMailboxWorld(t *testing.T, objects ...client.Object) *mailboxWorld {
	t.Helper()
	people := &realmPeople{realms: map[string][]keycloakRealmUser{}}
	kc := people.serve(t)
	scheme := deleteGapsScheme()
	objects = append(objects, mailCluster()...)
	objects = append(objects,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-demo"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: keycloakAdminSecret, Namespace: identityNamespace},
			Data: map[string][]byte{"url": []byte(kc.URL), "username": []byte("admin"), "password": []byte("x")}})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&gentianov1alpha1.MailboxRemoval{}).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel", MailServiceMode: mailServiceModeSystem}
	w := &mailboxWorld{t: t, c: c, tr: tr, people: people, now: mailboxT0.Add(time.Second)}
	w.r = &MailboxRemovalReconciler{Client: c, Tenant: tr, Now: func() time.Time { return w.now }}
	return w
}

func (w *mailboxWorld) reconcile(name string) *gentianov1alpha1.MailboxRemoval {
	w.t.Helper()
	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		w.t.Logf("reconcile %s: %v", name, err)
	}
	record := &gentianov1alpha1.MailboxRemoval{}
	if err := w.c.Get(context.Background(), types.NamespacedName{Name: name}, record); err != nil {
		w.t.Fatal(err)
	}
	return record
}

func (w *mailboxWorld) jobs() []batchv1.Job {
	w.t.Helper()
	list := &batchv1.JobList{}
	if err := w.c.List(context.Background(), list, client.InNamespace(mailNamespace)); err != nil {
		w.t.Fatal(err)
	}
	return list.Items
}

// finish ends a Job as its pod would: succeeded with a report, or failed.
func (w *mailboxWorld) finish(job *batchv1.Job, report string, failed bool) {
	w.t.Helper()
	ctx := context.Background()
	condition, phase, code := batchv1.JobComplete, corev1.PodSucceeded, int32(0)
	if failed {
		condition, phase, code = batchv1.JobFailed, corev1.PodFailed, 1
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	if err := w.c.Status().Update(ctx, job); err != nil {
		w.t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name}},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: report}}}}},
	}
	if err := w.c.Create(ctx, pod); err != nil {
		w.t.Fatal(err)
	}
}

func env(job *batchv1.Job) map[string]string {
	out := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

func person(address string, enabled bool) keycloakRealmUser {
	return keycloakRealmUser{Username: address, Email: address, Enabled: enabled}
}

// The whole of an archive: nothing is touched while the person is there;
// once they are gone their mail password goes at once, for the mail server
// and in the tenant's own copy, and the other person's stays; the mailbox is
// left alone until the server has read that; then one Job moves it, and the
// record says where it is and what it holds.
func TestARemovedPersonsMailboxIsArchived(t *testing.T) {
	ctx := context.Background()
	w := newMailboxWorld(t, selfhosted("demo"), mailboxRecord("demo-r1", "demo", "Alice@demo.k.example", gentianov1alpha1.MailboxChoiceArchive))
	tenant := &gentianov1alpha1.Tenant{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "demo"}, tenant); err != nil {
		t.Fatal(err)
	}
	w.people.set("demo", person("alice@demo.k.example", true), person("bob@demo.k.example", true))
	if err := w.tr.syncMailAppPasswords(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alice@demo.k.example", "bob@demo.k.example"} {
		if listed, err := w.tr.mailSignInListed(ctx, who); err != nil || !listed {
			t.Fatalf("%s has no mail password to begin with: %v", who, err)
		}
	}

	// The record is there and the person is too: the registrar has not
	// removed them yet.
	record := w.reconcile("demo-r1")
	if record.Status.Phase != gentianov1alpha1.MailboxRemovalPending || !strings.Contains(record.Status.Message, "still belongs to a person") || len(w.jobs()) != 0 {
		t.Fatalf("with the person still there: %+v, %d Job(s)", record.Status, len(w.jobs()))
	}

	// Removed.
	w.people.set("demo", person("bob@demo.k.example", true))
	record = w.reconcile("demo-r1")
	if record.Status.Phase != gentianov1alpha1.MailboxRemovalPending || record.Status.SignInGoneAt == nil || len(w.jobs()) != 0 {
		t.Fatalf("just after the removal: %+v, %d Job(s)", record.Status, len(w.jobs()))
	}
	if listed, _ := w.tr.mailSignInListed(ctx, "alice@demo.k.example"); listed {
		t.Error("the removed person's mail password is still with the mail server")
	}
	if listed, _ := w.tr.mailSignInListed(ctx, "bob@demo.k.example"); !listed {
		t.Error("the other person's mail password went with the removed person's")
	}
	plain := &corev1.Secret{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: mailAppPasswordTenantSec, Namespace: "tenant-demo"}, plain); err != nil {
		t.Fatal(err)
	}
	if _, kept := plain.Data[secretKeySafe("alice@demo.k.example")]; kept || len(plain.Data) != 1 {
		t.Errorf("the tenant's own copy of the mail passwords still holds the removed person's: %v", len(plain.Data))
	}

	// Not before the mail server has read it.
	w.now = w.now.Add(mailboxSignInSettle - time.Second)
	if w.reconcile("demo-r1"); len(w.jobs()) != 0 {
		t.Fatal("the mailbox was moved before the mail server could have read that nobody signs in")
	}
	w.now = w.now.Add(2 * time.Second)
	record = w.reconcile("demo-r1")
	jobs := w.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Job(s): %+v", len(jobs), record.Status)
	}
	job := &jobs[0]
	want := map[string]string{"DOMAIN": "demo.k.example", "BOX": "alice", "NAME": "alice-20261009T143000Z"}
	if got := env(job); got["DOMAIN"] != want["DOMAIN"] || got["BOX"] != want["BOX"] || got["NAME"] != want["NAME"] {
		t.Errorf("the Job works on %v, want %v", got, want)
	}
	if job.Name != backup.MailboxJobName("demo-r1", backup.MailboxActArchive) || job.Spec.Template.Spec.NodeSelector[corev1.LabelHostname] != "node-7" ||
		!strings.Contains(job.Spec.Template.Spec.Containers[0].Args[0], "mv -- ") {
		t.Errorf("not the archive Job on the mail server's node: %s %v", job.Name, job.Spec.Template.Spec.NodeSelector)
	}
	if record.Status.Phase != gentianov1alpha1.MailboxRemovalPending {
		t.Errorf("while the Job runs: %+v", record.Status)
	}

	// Still running: nothing changes.
	if record = w.reconcile("demo-r1"); record.Status.Phase != gentianov1alpha1.MailboxRemovalPending || len(w.jobs()) != 1 {
		t.Errorf("while the Job runs: %+v", record.Status)
	}
	w.finish(job, "result=archived\nbytes=20480\nmessages=3\n", false)
	record = w.reconcile("demo-r1")
	s := record.Status
	if s.Phase != gentianov1alpha1.MailboxRemovalArchived || s.Archive != "alice-20261009T143000Z" || s.Domain != "demo.k.example" ||
		s.SizeBytes != 20480 || s.Messages != 3 || s.ArchivedAt == nil || s.Refused {
		t.Fatalf("after the Job: %+v", s)
	}

	// --- later, on purpose: the archived mailbox is deleted.
	record.Spec.DeleteArchive = &gentianov1alpha1.MailboxRequester{Subject: "sub-admin2"}
	if err := w.c.Update(ctx, record); err != nil {
		t.Fatal(err)
	}
	w.reconcile("demo-r1")
	var purge *batchv1.Job
	for i, j := range w.jobs() {
		if j.Name == backup.MailboxJobName("demo-r1", backup.MailboxActDeleteArchive) {
			purge = &w.jobs()[i]
		}
	}
	if purge == nil {
		t.Fatal("no Job deletes the archived mailbox")
	}
	if got := env(purge); got["DOMAIN"] != "demo.k.example" || got["NAME"] != "alice-20261009T143000Z" || got["BOX"] != "" {
		t.Errorf("the Job that deletes the archive works on %v", got)
	}
	if script := purge.Spec.Template.Spec.Containers[0].Args[0]; !strings.Contains(script, `rm -rf -- "${ARCHIVE:?}/${DOMAIN:?}/${NAME:?}"`) {
		t.Error("the Job that deletes the archive removes something else")
	}
	w.finish(purge, "result=deleted\nbytes=20480\nmessages=3\n", false)
	record = w.reconcile("demo-r1")
	if record.Status.Phase != gentianov1alpha1.MailboxRemovalDeleted || record.Status.DeletedAt == nil || record.Status.ArchivedAt == nil {
		t.Errorf("after the archive was deleted: %+v", record.Status)
	}
	// Settled: nothing more happens to it.
	before := len(w.jobs())
	if w.reconcile("demo-r1"); len(w.jobs()) != before {
		t.Error("a settled record started a Job")
	}
}

// Deleted, and a mailbox that never was: both said as what they are.
func TestARemovedPersonsMailboxIsDeleted(t *testing.T) {
	for report, want := range map[string]gentianov1alpha1.MailboxRemovalPhase{
		"result=deleted\nbytes=4096\nmessages=2\n": gentianov1alpha1.MailboxRemovalDeleted,
		"result=none\nbytes=0\nmessages=0\n":       gentianov1alpha1.MailboxRemovalNoMailbox,
	} {
		w := newMailboxWorld(t, selfhosted("demo"), mailboxRecord("demo-r2", "demo", "bob@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
		w.people.set("demo", person("alice@demo.k.example", true))
		w.reconcile("demo-r2")
		w.now = w.now.Add(mailboxSignInSettle + time.Second)
		w.reconcile("demo-r2")
		jobs := w.jobs()
		if len(jobs) != 1 || jobs[0].Name != backup.MailboxJobName("demo-r2", backup.MailboxActDelete) {
			t.Fatalf("%d Job(s)", len(jobs))
		}
		if got := env(&jobs[0]); got["DOMAIN"] != "demo.k.example" || got["BOX"] != "bob" {
			t.Errorf("the Job deletes %v", got)
		}
		if script := jobs[0].Spec.Template.Spec.Containers[0].Args[0]; !strings.Contains(script, `rm -rf -- "${ROOT:?}/${DOMAIN:?}/${BOX:?}"`) {
			t.Error("not the script that deletes one mailbox")
		}
		w.finish(&jobs[0], report, false)
		if record := w.reconcile("demo-r2"); record.Status.Phase != want || record.Status.Archive != "" {
			t.Errorf("%q: %+v", report, record.Status)
		}
	}
}

// A mailbox that has a person is never touched, whatever a record says: not
// when the person was only switched off, not when the person is in another
// realm that shares the domain, and not when the realm cannot be asked.
func TestAMailboxThatHasAPersonIsNotTouched(t *testing.T) {
	// Switched off is not removed.
	w := newMailboxWorld(t, selfhosted("demo"), mailboxRecord("demo-r3", "demo", "carol@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
	w.people.set("demo", person("carol@demo.k.example", false))
	w.now = w.now.Add(mailboxPersonGrace - time.Minute)
	if record := w.reconcile("demo-r3"); record.Status.Phase != gentianov1alpha1.MailboxRemovalPending || record.Status.Refused {
		t.Fatalf("within the time the registrar has to remove the person: %+v", record.Status)
	}
	w.now = w.now.Add(2 * time.Minute)
	record := w.reconcile("demo-r3")
	if !record.Status.Refused || record.Status.Phase != gentianov1alpha1.MailboxRemovalFailed || !strings.Contains(record.Status.Message, "Nothing was changed") {
		t.Fatalf("a record for a person who is still there: %+v", record.Status)
	}
	// Refused is final: the person going later does not revive it.
	w.people.set("demo")
	w.now = w.now.Add(time.Hour)
	if w.reconcile("demo-r3"); len(w.jobs()) != 0 {
		t.Error("a refused record started a Job")
	}

	// The cluster's own domain, shared by the user tenant's realm and the
	// kernel realm: an administrator of the same address keeps the mailbox.
	user := selfhosted(gentianov1alpha1.SingleUserTenantName)
	w = newMailboxWorld(t, user, mailboxRecord("u-r1", user.Name, "dana@k.example", gentianov1alpha1.MailboxChoiceArchive))
	w.tr.TenancyMode = gentianov1alpha1.TenancyModeSingle
	w.people.set("kernel", keycloakRealmUser{Username: "dana", Email: "dana@k.example", Enabled: true})
	w.now = w.now.Add(mailboxPersonGrace + time.Minute)
	if record := w.reconcile("u-r1"); !record.Status.Refused || !strings.Contains(record.Status.Message, "realm kernel") || len(w.jobs()) != 0 {
		t.Errorf("an address the kernel realm's administrator holds: %+v", record.Status)
	}

	// The identity provider does not answer: nothing is assumed.
	w = newMailboxWorld(t, selfhosted("demo"), mailboxRecord("demo-r4", "demo", "erin@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
	w.people.down = true
	w.now = w.now.Add(time.Hour)
	record = w.reconcile("demo-r4")
	if record.Status.Refused || record.Status.Phase != gentianov1alpha1.MailboxRemovalPending || !strings.Contains(record.Status.Message, "could not be listed") || len(w.jobs()) != 0 {
		t.Errorf("with the identity provider down: %+v", record.Status)
	}
}

// One person's mailbox on the cluster's own domain is that person's: on a
// cluster with one user tenant the choice is carried out there too, on the
// one directory, where the tenant's backup and deletion leave the domain
// alone.
func TestOnePersonsMailboxOnASharedDomainIsCarriedOut(t *testing.T) {
	user := selfhosted(gentianov1alpha1.SingleUserTenantName)
	w := newMailboxWorld(t, user, mailboxRecord("u-r2", user.Name, "dana@k.example", gentianov1alpha1.MailboxChoiceArchive))
	w.tr.TenancyMode = gentianov1alpha1.TenancyModeSingle
	if boxes, why, _ := w.tr.mailboxesOf(context.Background(), user); boxes != nil || why == "" {
		t.Fatalf("the domain is not shared in this test: %+v %q", boxes, why)
	}
	w.reconcile("u-r2")
	w.now = w.now.Add(mailboxSignInSettle + time.Second)
	w.reconcile("u-r2")
	jobs := w.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Job(s)", len(jobs))
	}
	if got := env(&jobs[0]); got["DOMAIN"] != "k.example" || got["BOX"] != "dana" {
		t.Errorf("the Job works on %v", got)
	}
}

// A record is written by a process that takes requests. What it names is
// checked against what the tenant has, and a name that is not one of the
// tenant's mailboxes is refused with nothing changed.
func TestARecordThatNamesSomethingElseIsRefused(t *testing.T) {
	for _, address := range []string{
		"alice@other.k.example", // another tenant's domain
		"alice@k.example",       // the cluster's own
		"../other.k.example/carol@demo.k.example",
		"a/b@demo.k.example",
		".archive@demo.k.example",
		"@demo.k.example",
		"alice",
	} {
		w := newMailboxWorld(t, selfhosted("demo"), selfhosted("other"), mailboxRecord("demo-bad", "demo", address, gentianov1alpha1.MailboxChoiceDelete))
		w.now = w.now.Add(time.Hour)
		record := w.reconcile("demo-bad")
		if !record.Status.Refused || len(w.jobs()) != 0 {
			t.Errorf("%q: %+v, %d Job(s)", address, record.Status, len(w.jobs()))
		}
	}
}

// A tenant whose mail is not on the cluster's own mail server has no
// mailbox to decide about, and a cluster that runs no mail server has none.
func TestNoMailboxWhereTheClusterKeepsNone(t *testing.T) {
	relay := planTenant("demo")
	relay.Spec.Mail = &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeTransportOnly}
	w := newMailboxWorld(t, relay, mailboxRecord("demo-r5", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
	if got := w.tr.mailboxDomainOf(context.Background(), relay); got != "" {
		t.Errorf("a tenant that only sends has the mailbox domain %q", got)
	}
	if record := w.reconcile("demo-r5"); record.Status.Phase != gentianov1alpha1.MailboxRemovalNoMailbox || len(w.jobs()) != 0 {
		t.Errorf("%+v", record.Status)
	}

	w = newMailboxWorld(t, selfhosted("demo"), mailboxRecord("demo-r6", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
	w.tr.MailServiceMode = "external"
	if got := w.tr.mailboxDomainOf(context.Background(), selfhosted("demo")); got != "" {
		t.Errorf("a cluster with no mail server of its own gives a tenant the mailbox domain %q", got)
	}
	if record := w.reconcile("demo-r6"); record.Status.Phase != gentianov1alpha1.MailboxRemovalNoMailbox || len(w.jobs()) != 0 {
		t.Errorf("%+v", record.Status)
	}
	if got := w.tr.mailboxDomainOf(context.Background(), selfhosted("demo")); got != "" {
		t.Error(got)
	}
	w.tr.MailServiceMode = mailServiceModeSystem
	if got := w.tr.mailboxDomainOf(context.Background(), selfhosted("demo")); got != "demo.k.example" {
		t.Errorf("a tenant with its mail on the cluster's mail server has the mailbox domain %q", got)
	}
}

// A Job that fails is said on the record, with why, and run again -- later
// each time, and counted once.
func TestAFailedMailboxJobIsSaidAndRunAgain(t *testing.T) {
	w := newMailboxWorld(t, selfhosted("demo"), mailboxRecord("demo-r7", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
	w.reconcile("demo-r7")
	w.now = w.now.Add(mailboxSignInSettle + time.Second)
	w.reconcile("demo-r7")
	jobs := w.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Job(s)", len(jobs))
	}
	w.finish(&jobs[0], "the volume could not be listed", true)
	record := w.reconcile("demo-r7")
	if record.Status.Phase != gentianov1alpha1.MailboxRemovalFailed || record.Status.Refused || record.Status.Attempts != 1 ||
		!strings.Contains(record.Status.Message, "the volume could not be listed") || !strings.Contains(record.Status.Message, "tried again") {
		t.Fatalf("after the failure: %+v", record.Status)
	}
	if len(w.jobs()) != 0 {
		t.Fatal("the failed Job was left, and the next attempt would find it")
	}
	// Not at once.
	w.now = w.now.Add(30 * time.Second)
	if record = w.reconcile("demo-r7"); len(w.jobs()) != 0 || record.Status.Attempts != 1 {
		t.Fatalf("the next attempt started at once: %+v", record.Status)
	}
	w.now = w.now.Add(time.Minute)
	record = w.reconcile("demo-r7")
	jobs = w.jobs()
	if len(jobs) != 1 || record.Status.Attempts != 1 || record.Status.Phase != gentianov1alpha1.MailboxRemovalFailed {
		t.Fatalf("the next attempt: %d Job(s), %+v", len(jobs), record.Status)
	}
	if err := w.c.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: jobs[0].Name + "-x", Namespace: mailNamespace}}); err != nil {
		t.Fatal(err)
	}
	w.finish(&jobs[0], "result=deleted\nbytes=1024\nmessages=1\n", false)
	if record = w.reconcile("demo-r7"); record.Status.Phase != gentianov1alpha1.MailboxRemovalDeleted {
		t.Errorf("after the second attempt: %+v", record.Status)
	}
}

// The last decision about a mailbox is the one carried out. One written for
// a removal that then failed -- the person stayed -- does not come alive
// when the person is removed later with the other answer.
func TestOnlyTheLastDecisionAboutAMailboxIsCarriedOut(t *testing.T) {
	stale := mailboxRecord("demo-old", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete)
	later := mailboxRecord("demo-new", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceArchive)
	later.CreationTimestamp = metav1.NewTime(mailboxT0.Add(30 * time.Second))
	w := newMailboxWorld(t, selfhosted("demo"), stale, later)
	w.now = mailboxT0.Add(time.Minute)
	if record := w.reconcile("demo-old"); !record.Status.Refused || !strings.Contains(record.Status.Message, "a later decision") {
		t.Fatalf("the earlier decision: %+v", record.Status)
	}
	w.reconcile("demo-new")
	w.now = w.now.Add(mailboxSignInSettle + time.Second)
	w.reconcile("demo-old")
	w.reconcile("demo-new")
	jobs := w.jobs()
	if len(jobs) != 1 || jobs[0].Name != backup.MailboxJobName("demo-new", backup.MailboxActArchive) {
		t.Fatalf("the Jobs are not the last decision's alone: %d", len(jobs))
	}
}

// The record of a mailbox that is gone is not kept for ever: it names a
// person's address. An archived mailbox's record stays as long as the
// archive.
func TestTheRecordOfAMailboxThatIsGoneIsRemovedInTime(t *testing.T) {
	ctx := context.Background()
	gone := mailboxRecord("demo-gone", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete)
	kept := mailboxRecord("demo-kept", "demo", "bob@demo.k.example", gentianov1alpha1.MailboxChoiceArchive)
	w := newMailboxWorld(t, selfhosted("demo"), gone, kept)
	deleted := metav1.NewTime(mailboxT0.Add(time.Hour))
	gone.Status = gentianov1alpha1.MailboxRemovalStatus{Phase: gentianov1alpha1.MailboxRemovalDeleted, DeletedAt: &deleted}
	kept.Status = gentianov1alpha1.MailboxRemovalStatus{Phase: gentianov1alpha1.MailboxRemovalArchived, Archive: "bob-20261009T143000Z", Domain: "demo.k.example"}
	for _, r := range []*gentianov1alpha1.MailboxRemoval{gone, kept} {
		if err := w.c.Status().Update(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	w.now = deleted.Add(mailboxRecordKept - time.Hour)
	w.reconcile("demo-gone")
	w.now = deleted.Add(mailboxRecordKept + 365*24*time.Hour)
	for _, name := range []string{"demo-gone", "demo-kept"} {
		_, _ = w.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	}
	left := &gentianov1alpha1.MailboxRemovalList{}
	if err := w.c.List(ctx, left); err != nil || len(left.Items) != 1 || left.Items[0].Name != "demo-kept" {
		t.Errorf("the records left are %v, %v", left.Items, err)
	}
}

// The archived mailboxes travel: a backup names them in its manifest beside
// the mail it copies, a restore plans them into the mail domain of the
// tenant restored into, puts each on record there once, and the operator
// reports such a record as what it is -- an archive that is already there --
// and runs no Job for it. Deleting it later is the same act as for any.
func TestArchivedMailboxesTravelAsArchives(t *testing.T) {
	ctx := context.Background()
	at := metav1.NewTime(mailboxT0.Add(time.Hour))
	archived := mailboxRecord("demo-arch", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceArchive)
	pending := mailboxRecord("demo-pend", "demo", "bob@demo.k.example", gentianov1alpha1.MailboxChoiceArchive)
	elsewhere := mailboxRecord("other-arch", "other", "alice@other.k.example", gentianov1alpha1.MailboxChoiceArchive)
	w := newMailboxWorld(t, selfhosted("demo"), selfhosted("fresh"), archived, pending, elsewhere)
	for record, status := range map[*gentianov1alpha1.MailboxRemoval]gentianov1alpha1.MailboxRemovalStatus{
		archived:  {Phase: gentianov1alpha1.MailboxRemovalArchived, Archive: "alice-20261009T143000Z", Domain: "demo.k.example", ArchivedAt: &at, SizeBytes: 4096, Messages: 3},
		elsewhere: {Phase: gentianov1alpha1.MailboxRemovalArchived, Archive: "alice-20261009T143000Z", Domain: "other.k.example"},
	} {
		record.Status = status
		if err := w.c.Status().Update(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	er := &TenantExportReconciler{Client: w.c, Reconciler: w.tr}
	export := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"}}}
	demo := selfhosted("demo")
	if got, err := er.archivedMailboxes(ctx, export, demo); err != nil || got != nil {
		t.Fatalf("an export that captured no mailboxes names archived ones: %v, %v", got, err)
	}
	export.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: backupTenantComponent, Artefacts: []gentianov1alpha1.BundleArtefact{
		{Kind: bundle.ArtefactMailboxes, Name: "demo.k.example", Path: backup.MailboxesArtefact}}}}
	held, err := er.archivedMailboxes(ctx, export, demo)
	if err != nil || len(held) != 1 {
		t.Fatalf("the manifest names %+v, %v: want the tenant's one archived mailbox, not the pending one and not another tenant's", held, err)
	}
	if held[0] != (bundle.ArchivedMailbox{Archive: "alice-20261009T143000Z", Address: "alice@demo.k.example", ArchivedAt: at.UTC().Format(time.RFC3339),
		By: "admin@demo.k.example", SizeBytes: 4096, Messages: 3}) {
		t.Errorf("named as %+v", held[0])
	}
	unit, err := er.manifestUnitWith(export, demo, gapsEncryption, nil, held)
	if err != nil || !strings.Contains(containerArgs(unit.Job), `"archivedMailboxes":[{"archive":"alice-20261009T143000Z","address":"alice@demo.k.example"`) {
		t.Fatalf("the manifest does not carry them: %v", err)
	}

	// Planned into the domain of the tenant restored into. What a bundle
	// names that is not an archived mailbox is left out.
	held = append(held,
		bundle.ArchivedMailbox{Archive: "../../other.k.example", Address: "x@demo.k.example"},
		bundle.ArchivedMailbox{Archive: "bob-20261009T143000Z", Address: "alice@demo.k.example"},
		bundle.ArchivedMailbox{Archive: "carol-20261009T143000Z", Address: "../carol@demo.k.example"})
	planned := archivedMailboxesPlan(held, "fresh.k.example")
	if len(planned) != 1 || planned[0].Address != "alice@fresh.k.example" || planned[0].Domain != "fresh.k.example" || planned[0].Archive != "alice-20261009T143000Z" ||
		planned[0].ArchivedAt == nil || !planned[0].ArchivedAt.Time.Equal(at.Time) || planned[0].Messages != 3 {
		t.Fatalf("planned as %+v", planned)
	}

	// Put on record once, however often the restore comes by.
	rr := &TenantRestoreReconciler{Client: w.c, Tenant: w.tr, Reconciler: er}
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "import-1", Namespace: "tenant-fresh"},
		Status: gentianov1alpha1.TenantRestoreStatus{ArchivedMailboxes: planned}}
	fresh := selfhosted("fresh")
	for i := 0; i < 2; i++ {
		if err := rr.recordArchivedMailboxes(ctx, fresh, restore); err != nil {
			t.Fatal(err)
		}
	}
	all := &gentianov1alpha1.MailboxRemovalList{}
	if err := w.c.List(ctx, all); err != nil {
		t.Fatal(err)
	}
	var brought []gentianov1alpha1.MailboxRemoval
	for _, r := range all.Items {
		if r.Spec.Tenant == "fresh" {
			brought = append(brought, r)
		}
	}
	if len(brought) != 1 || brought[0].Spec.Restored == nil || brought[0].Spec.Mailbox != gentianov1alpha1.MailboxChoiceArchive ||
		brought[0].Spec.Address != "alice@fresh.k.example" || brought[0].Spec.RequestedBy.Subject != "restore:import-1" {
		t.Fatalf("on record in the new tenant: %+v", brought)
	}

	// The operator reports it and touches nothing -- although nobody of the
	// new tenant holds the address and every wait has long passed.
	w.now = w.now.Add(24 * time.Hour)
	record := w.reconcile(brought[0].Name)
	s := record.Status
	if s.Phase != gentianov1alpha1.MailboxRemovalArchived || s.Archive != "alice-20261009T143000Z" || s.Domain != "fresh.k.example" || s.Messages != 3 || len(w.jobs()) != 0 {
		t.Fatalf("a restored archive's record: %+v, %d Job(s)", s, len(w.jobs()))
	}
	// And deleting it is the Job that deletes an archive, in the new domain.
	record.Spec.DeleteArchive = &gentianov1alpha1.MailboxRequester{Subject: "sub-admin"}
	if err := w.c.Update(ctx, record); err != nil {
		t.Fatal(err)
	}
	w.reconcile(record.Name)
	jobs := w.jobs()
	if len(jobs) != 1 || jobs[0].Name != backup.MailboxJobName(record.Name, backup.MailboxActDeleteArchive) || env(&jobs[0])["DOMAIN"] != "fresh.k.example" {
		t.Fatalf("deleting a restored archive: %d Job(s)", len(jobs))
	}

	// A restored record that names something else is refused.
	bad := mailboxRecord("fresh-bad", "fresh", "x@fresh.k.example", gentianov1alpha1.MailboxChoiceArchive)
	bad.Spec.Restored = &gentianov1alpha1.RestoredArchive{Archive: "../x", Domain: "fresh.k.example"}
	if err := w.c.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if got := w.reconcile("fresh-bad"); !got.Status.Refused {
		t.Errorf("%+v", got.Status)
	}
}

// A tenant deleted with its data takes the records of its removed people's
// mailboxes with it, once the Job that destroys the domain's mailboxes and
// archive has succeeded; another tenant's stay.
func TestATenantsDeletionForgetsItsArchivedMailboxes(t *testing.T) {
	ctx := context.Background()
	tenant := selfhosted("demo")
	tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	w := newMailboxWorld(t, tenant,
		mailboxRecord("demo-a", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceArchive),
		mailboxRecord("other-a", "other", "alice@other.k.example", gentianov1alpha1.MailboxChoiceArchive))
	if err := w.tr.deleteMailboxes(ctx, tenant); err != errDeleteJobPending {
		t.Fatalf("the deletion did not wait for the mailboxes' Job: %v", err)
	}
	records := &gentianov1alpha1.MailboxRemovalList{}
	if err := w.c.List(ctx, records); err != nil || len(records.Items) != 2 {
		t.Fatalf("a record went before the archive did: %d, %v", len(records.Items), err)
	}
	job := &batchv1.Job{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: backup.MailboxDestroyJobName("demo"), Namespace: mailNamespace}, job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(job.Spec.Template.Spec.Containers[0].Args[0], backup.MailArchiveDir) {
		t.Error("the Job that destroys a tenant's mailboxes leaves its archived ones")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := w.c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := w.tr.deleteMailboxes(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := w.c.List(ctx, records); err != nil || len(records.Items) != 1 || records.Items[0].Name != "other-a" {
		t.Errorf("after the deletion the records are %v, %v", records.Items, err)
	}
}
