/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// The question at removal: what becomes of the person's mailbox.

// fakeMailboxes is the record of decisions, in memory.
type fakeMailboxes struct {
	mu        sync.Mutex
	decided   []registrar.MailboxDecision
	withdrawn []string
	deletions []string
	listed    map[string][]registrar.RemovedMailbox
	decideErr error
	log       *[]string
}

// newFakeMailboxes has one archived mailbox of tenant demo and one of
// tenant other.
func newFakeMailboxes() *fakeMailboxes {
	at := time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC)
	return &fakeMailboxes{listed: map[string][]registrar.RemovedMailbox{
		"demo": {
			{ID: "demo-arch1", Address: "gone@demo." + dt.KernelDomain, Choice: "archive", State: "archived", RemovedAt: at, By: "tom@example.com", SizeBytes: 2048, Messages: 3},
			{ID: "demo-del1", Address: "left@demo." + dt.KernelDomain, Choice: "delete", State: "deleted", RemovedAt: at.Add(-time.Hour)},
		},
		"other": {{ID: "other-arch1", Address: "x@other." + dt.KernelDomain, Choice: "archive", State: "archived", RemovedAt: at}},
	}}
}

func (f *fakeMailboxes) Decide(_ context.Context, d registrar.MailboxDecision) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decideErr != nil {
		return "", f.decideErr
	}
	f.decided = append(f.decided, d)
	if f.log != nil {
		*f.log = append(*f.log, "decided "+d.Choice+" for "+d.Address)
	}
	return d.Tenant + "-new1", nil
}

func (f *fakeMailboxes) Withdraw(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withdrawn = append(f.withdrawn, id)
	return nil
}

func (f *fakeMailboxes) List(_ context.Context, tenant string) ([]registrar.RemovedMailbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registrar.RemovedMailbox{}, f.listed[tenant]...), nil
}

func (f *fakeMailboxes) DeleteArchive(_ context.Context, tenant, id string, by registrar.MailboxDecision) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.listed[tenant] {
		if m.ID != id {
			continue
		}
		if m.State != "archived" {
			return "", registrar.ErrNotArchived
		}
		f.deletions = append(f.deletions, id+" by "+by.Subject+" "+by.Name)
		return m.Address, nil
	}
	return "", registrar.ErrMailboxNotFound
}

// withMail is tenant demo with its people's mailboxes on the cluster's own
// mail server, and tenant solo without.
func withMail() fakeTenants {
	demo := ownRealm("demo")
	demo.MailboxDomain = demo.LoginDomain
	return fakeTenants{"demo": demo, "solo": ownRealm("solo")}
}

func mailPeople(f *fakeIdentity) {
	f.people = map[string]identity.Person{
		// A person of the tenant's own domain: a mailbox.
		"u1": {ID: "u1", Username: "Ada@demo." + dt.KernelDomain, Email: "ada@elsewhere.example", Enabled: true},
		// A login that is no address, with an address in the domain.
		"u2": {ID: "u2", Username: "bert", Email: "bert@demo." + dt.KernelDomain, Enabled: true},
		// Somebody who signs in under an address elsewhere: none here.
		"u3": {ID: "u3", Username: "cleo@partner.example", Email: "cleo@partner.example", Enabled: true},
	}
}

// Removing a person who has a mailbox requires the answer, and has no
// default: without it, or with anything that is not one of the two words,
// the request is refused, says why, and has changed nothing.
func TestRemovingAPersonWithAMailboxRequiresTheChoice(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	mailPeople(f)
	boxes := newFakeMailboxes()
	h := startWithTenants(t, f, nil, withMail(), boxes)
	tok := h.token(t, "tenant-demo", "tom")
	for _, body := range []string{`{"person":"u1"}`, `{"person":"u1","mailbox":""}`, `{"person":"u1","mailbox":"keep"}`, `{"person":"u1","mailbox":"Archive"}`, `{"person":"u2"}`} {
		status, answer := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tok, body)
		said, _ := answer["error"].(string)
		if status != http.StatusBadRequest || !strings.Contains(said, `"archive"`) || !strings.Contains(said, `"delete"`) ||
			!strings.Contains(said, "cannot be undone") || !strings.Contains(said, "Nothing was changed") {
			t.Errorf("%s: %d %q", body, status, said)
		}
	}
	if !strings.Contains(func() string {
		_, answer := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tok, `{"person":"u1"}`)
		said, _ := answer["error"].(string)
		return said
	}(), "ada@demo."+dt.KernelDomain) {
		t.Error("the refusal does not name the mailbox")
	}
	if len(f.removed) != 0 || len(boxes.decided) != 0 {
		t.Fatalf("a refused removal removed %v and decided %v", f.removed, boxes.decided)
	}
}

// With the answer: it is written down first, with who gave it, and the
// person is removed then.
func TestTheChoiceIsWrittenDownBeforeThePersonIsRemoved(t *testing.T) {
	for _, choice := range []string{"archive", "delete"} {
		var order []string
		f := newFakeIdentity("demo", "solo")
		mailPeople(f)
		f.log = &order
		boxes := newFakeMailboxes()
		boxes.log = &order
		h := startWithTenants(t, f, nil, withMail(), boxes)
		tok := h.token(t, "tenant-demo", "tom")
		status, answer := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tok, `{"person":"u1","mailbox":"`+choice+`"}`)
		if status != http.StatusOK || answer["removed"] != true {
			t.Fatalf("%s: %d %v", choice, status, answer)
		}
		address := "ada@demo." + dt.KernelDomain
		if len(order) != 2 || order[0] != "decided "+choice+" for "+address || order[1] != "removed u1" {
			t.Errorf("%s: the order was %v", choice, order)
		}
		d := boxes.decided[0]
		if d.Tenant != "demo" || d.Address != address || d.Choice != choice || d.Subject != "tom" || d.Name != "tom@example.com" || d.RequestID == "" {
			t.Errorf("%s: written down as %+v", choice, d)
		}
		box, _ := answer["mailbox"].(map[string]any)
		if box["address"] != address || box["choice"] != choice || box["id"] != "demo-new1" || box["state"] != "pending" {
			t.Errorf("%s: the answer says %v", choice, answer["mailbox"])
		}
	}
}

// Where there is no mailbox nothing is asked: a tenant that keeps none on
// the cluster's mail server, and a person whose address is elsewhere. The
// field, sent anyway, is not read.
func TestNothingIsAskedWhereThereIsNoMailbox(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	mailPeople(f)
	boxes := newFakeMailboxes()
	for _, c := range []struct{ realm, who, path, body string }{
		{"tenant-solo", "tina", "/v1/tenants/solo/actions/remove-person", `{"person":"u1"}`},
		{"tenant-solo", "tina", "/v1/tenants/solo/actions/remove-person", `{"person":"u2","mailbox":"delete"}`},
		{"tenant-demo", "tom", "/v1/tenants/demo/actions/remove-person", `{"person":"u3"}`},
		{"tenant-demo", "tom", "/v1/tenants/demo/actions/remove-person", `{"person":"u3","mailbox":"anything"}`},
	} {
		table := dt.Table{"user:tina can_manage_users tenant:solo": true, "user:tom can_manage_users tenant:demo": true}
		h := startWithTenants(t, f, dt.Checker(t, table), withMail(), boxes)
		status, answer := h.do(t, http.MethodPost, c.path, h.token(t, c.realm, c.who), c.body)
		if status != http.StatusOK || answer["mailbox"] != nil {
			t.Errorf("%s %s: %d %v", c.path, c.body, status, answer)
		}
	}
	if len(boxes.decided) != 0 {
		t.Errorf("a decision was written down for no mailbox: %+v", boxes.decided)
	}
	if len(f.removed) != 4 {
		t.Errorf("removed %v", f.removed)
	}
}

// Switching a person off is not removing them: it asks nothing and nothing
// is written down about their mailbox.
func TestSwitchingAPersonOffAsksNothingAboutTheirMailbox(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	mailPeople(f)
	boxes := newFakeMailboxes()
	h := startWithTenants(t, f, nil, withMail(), boxes)
	status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/update-person", h.token(t, "tenant-demo", "tom"), `{"person":"u1","enabled":false}`)
	if status != http.StatusOK || len(boxes.decided) != 0 {
		t.Errorf("switching off: %d, decided %v", status, boxes.decided)
	}
}

// A decision that cannot be written down removes nobody; a removal that
// fails takes its decision back.
func TestADecisionAndARemovalGoTogether(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	mailPeople(f)
	boxes := newFakeMailboxes()
	boxes.decideErr = errors.New("the API server did not answer")
	h := startWithTenants(t, f, nil, withMail(), boxes)
	tok := h.token(t, "tenant-demo", "tom")
	status, answer := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tok, `{"person":"u1","mailbox":"delete"}`)
	if status != http.StatusServiceUnavailable || len(f.removed) != 0 || !strings.Contains(answer["error"].(string), "not removed") {
		t.Fatalf("with the record unavailable: %d %v, removed %v", status, answer, f.removed)
	}

	// No record at all: the same.
	h = startWithTenants(t, f, nil, withMail(), nil)
	status, _ = h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", h.token(t, "tenant-demo", "tom"), `{"person":"u1","mailbox":"delete"}`)
	if status != http.StatusServiceUnavailable || len(f.removed) != 0 {
		t.Fatalf("with no record configured: %d, removed %v", status, f.removed)
	}

	boxes = newFakeMailboxes()
	f.removeErr = identity.ErrProtected
	h = startWithTenants(t, f, nil, withMail(), boxes)
	status, _ = h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", h.token(t, "tenant-demo", "tom"), `{"person":"u1","mailbox":"delete"}`)
	if status != http.StatusForbidden || len(boxes.decided) != 1 || len(boxes.withdrawn) != 1 || boxes.withdrawn[0] != "demo-new1" {
		t.Fatalf("a removal that failed: %d, decided %v, withdrawn %v", status, boxes.decided, boxes.withdrawn)
	}
}

// The people a screen lists say who has a mailbox, so that it knows whom to
// ask about.
func TestThePeopleSayWhoHasAMailbox(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	mailPeople(f)
	h := startWithTenants(t, f, nil, withMail(), newFakeMailboxes())
	tok := h.token(t, "tenant-demo", "tom")
	for id, want := range map[string]any{"u1": "ada@demo." + dt.KernelDomain, "u2": "bert@demo." + dt.KernelDomain, "u3": nil} {
		status, person := h.do(t, http.MethodGet, "/v1/tenants/demo/people/"+id, tok, "")
		if status != http.StatusOK || person["mailbox"] != want {
			t.Errorf("%s: %d mailbox = %v, want %v", id, status, person["mailbox"], want)
		}
	}
}

// What became of the removed people's mailboxes is read per tenant, and an
// archived one is deleted on purpose, by whoever may remove people -- of
// this tenant, and no other's.
func TestTheRemovedMailboxesAreListedAndAnArchiveIsDeleted(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	boxes := newFakeMailboxes()
	h := startWithTenants(t, f, nil, withMail(), boxes)
	tok := h.token(t, "tenant-demo", "tom")
	status, answer := h.do(t, http.MethodGet, "/v1/tenants/demo/removed-mailboxes", tok, "")
	list, _ := answer["mailboxes"].([]any)
	if status != http.StatusOK || len(list) != 2 || answer["mailboxDomain"] != "demo."+dt.KernelDomain {
		t.Fatalf("%d %v", status, answer)
	}
	first := list[0].(map[string]any)
	if first["id"] != "demo-arch1" || first["state"] != "archived" || first["by"] != "tom@example.com" || first["sizeBytes"] != float64(2048) || first["address"] != "gone@demo."+dt.KernelDomain {
		t.Errorf("the archived mailbox is listed as %v", first)
	}

	del := "/v1/tenants/demo/actions/delete-archived-mailbox"
	for body, want := range map[string]int{
		`{"mailbox":"demo-arch1"}`:  http.StatusAccepted,
		`{"mailbox":"demo-del1"}`:   http.StatusConflict,
		`{"mailbox":"other-arch1"}`: http.StatusNotFound,
		`{"mailbox":"nothing"}`:     http.StatusNotFound,
		`{"mailbox":""}`:            http.StatusBadRequest,
		`{"mailbox":"../x"}`:        http.StatusBadRequest,
		`{}`:                        http.StatusBadRequest,
	} {
		if status, answer := h.do(t, http.MethodPost, del, tok, body); status != want {
			t.Errorf("%s: %d %v, want %d", body, status, answer, want)
		}
	}
	if len(boxes.deletions) != 1 || boxes.deletions[0] != "demo-arch1 by tom tom@example.com" {
		t.Errorf("deletions asked for: %v", boxes.deletions)
	}
}

// The record in the cluster: what the registrar writes for the operator,
// and reads back.
func TestTheDecisionIsAnObjectTheOperatorReads(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithStatusSubresource(&gentianov1alpha1.MailboxRemoval{}).Build()
	boxes := &registrar.ClusterMailboxes{Client: c}

	if _, err := boxes.Decide(ctx, registrar.MailboxDecision{Tenant: "demo", Address: "a@demo.example", Choice: "keep"}); err == nil {
		t.Fatal("a choice that is neither archive nor delete was written down")
	}
	id, err := boxes.Decide(ctx, registrar.MailboxDecision{Tenant: "demo", Address: "Ada@demo.example", Choice: registrar.MailboxArchive,
		Subject: "sub-tom", Name: "tom@example.com", RequestID: "req-12345678"})
	if err != nil {
		t.Fatal(err)
	}
	var all gentianov1alpha1.MailboxRemovalList
	if err := c.List(ctx, &all); err != nil || len(all.Items) != 1 {
		t.Fatalf("%d record(s), %v", len(all.Items), err)
	}
	record := &all.Items[0]
	// The fake API server names nothing; the real one completes the name.
	if record.GenerateName != "demo-" || record.Labels["gentianos.io/tenant"] != "demo" {
		t.Errorf("named %q %v", record.GenerateName, record.Labels)
	}
	spec := record.Spec
	if spec.Tenant != "demo" || spec.Address != "ada@demo.example" || spec.Mailbox != gentianov1alpha1.MailboxChoiceArchive ||
		spec.RequestedBy.Subject != "sub-tom" || spec.RequestedBy.Name != "tom@example.com" || spec.RequestedBy.RequestID != "req-12345678" || spec.RequestedBy.At == nil || spec.DeleteArchive != nil {
		t.Errorf("written down as %+v", spec)
	}
	_ = id

	// Read back as the operator leaves it.
	other := &gentianov1alpha1.MailboxRemoval{ObjectMeta: metav1.ObjectMeta{Name: "other-1"},
		Spec: gentianov1alpha1.MailboxRemovalSpec{Tenant: "other", Address: "x@other.example", Mailbox: gentianov1alpha1.MailboxChoiceDelete}}
	if err := c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	list, err := boxes.List(ctx, "demo")
	if err != nil || len(list) != 1 || list[0].State != "pending" || list[0].Address != "ada@demo.example" || list[0].By != "tom@example.com" {
		t.Fatalf("%+v %v", list, err)
	}
	if _, err := boxes.DeleteArchive(ctx, "demo", record.Name, registrar.MailboxDecision{Subject: "s"}); !errors.Is(err, registrar.ErrNotArchived) {
		t.Errorf("a mailbox that is not archived yet: %v", err)
	}
	at := metav1.NewTime(time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC))
	record.Status = gentianov1alpha1.MailboxRemovalStatus{Phase: gentianov1alpha1.MailboxRemovalArchived, Archive: "ada-20261009T143000Z", ArchivedAt: &at, SizeBytes: 4096, Messages: 7, Message: "Archived"}
	if err := c.Status().Update(ctx, record); err != nil {
		t.Fatal(err)
	}
	list, _ = boxes.List(ctx, "demo")
	if len(list) != 1 || list[0].State != "archived" || list[0].SizeBytes != 4096 || list[0].Messages != 7 || list[0].ArchivedAt == nil || list[0].DeletionRequested {
		t.Fatalf("%+v", list)
	}

	// Deleting the archive is asked for on the record, by name and for its
	// own tenant only.
	if _, err := boxes.DeleteArchive(ctx, "other", record.Name, registrar.MailboxDecision{Subject: "s"}); !errors.Is(err, registrar.ErrMailboxNotFound) {
		t.Errorf("another tenant asked for this tenant's archive to be deleted: %v", err)
	}
	if _, err := boxes.DeleteArchive(ctx, "demo", "nothing", registrar.MailboxDecision{Subject: "s"}); !errors.Is(err, registrar.ErrMailboxNotFound) {
		t.Errorf("a record that is not there: %v", err)
	}
	address, err := boxes.DeleteArchive(ctx, "demo", record.Name, registrar.MailboxDecision{Subject: "sub-tina", Name: "tina@example.com", RequestID: "req-2"})
	if err != nil || address != "ada@demo.example" {
		t.Fatalf("%q %v", address, err)
	}
	got := &gentianov1alpha1.MailboxRemoval{}
	if err := c.Get(ctx, types.NamespacedName{Name: record.Name}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DeleteArchive == nil || got.Spec.DeleteArchive.Subject != "sub-tina" || got.Spec.Mailbox != gentianov1alpha1.MailboxChoiceArchive {
		t.Errorf("after the request: %+v", got.Spec)
	}
	list, _ = boxes.List(ctx, "demo")
	if !list[0].DeletionRequested || list[0].DeletionBy != "tina@example.com" {
		t.Errorf("the list does not say the deletion was asked for: %+v", list[0])
	}

	// Taken back: gone, and taking it back twice is no error.
	for i := 0; i < 2; i++ {
		if err := boxes.Withdraw(ctx, record.Name); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ = boxes.List(ctx, "demo"); len(list) != 0 {
		t.Errorf("a withdrawn decision is still listed: %+v", list)
	}
}

// Where the tenant keeps its people's mailboxes is the operator's to say,
// on the tenant.
func TestTheMailboxDomainIsReadOffTheTenant(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		tenant("acme", func(t *gentianov1alpha1.Tenant) { t.Status.MailboxDomain = "Acme.example.test" }),
		tenant("relay", nil),
	).Build()
	tenants := &registrar.ClusterTenants{Client: c, KernelDomain: "example.test"}
	for name, want := range map[string]string{"acme": "acme.example.test", "relay": ""} {
		got, err := tenants.Tenant(context.Background(), name)
		if err != nil || got.MailboxDomain != want {
			t.Errorf("%s: mailbox domain %q, %v; want %q", name, got.MailboxDomain, err, want)
		}
	}
}
