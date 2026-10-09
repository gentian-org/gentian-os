/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// What becomes of a removed person's mailbox.
//
// On a cluster that runs its own mail server a person of a tenant has a
// mailbox there, and removing the person leaves the question of the mail.
// It is not answered by a default. Whoever removes the person is asked,
// then, and says one of two things: archive it, or delete it.
//
// The registrar takes the answer and does nothing to the mailbox. It cannot:
// it holds no right in the mail server's namespace and mounts nothing of its
// volume, and is not to. It writes the answer down, as a MailboxRemoval in
// the cluster -- the one kind it may write -- and the operator, which holds
// the rights that move and destroy mail, carries it out and reports on the
// same object what happened. The registrar reads that back for the console.
//
// The answer is written down before the person is removed. A person removed
// with the answer lost would leave a mailbox nobody decided about and nobody
// is asked about again; the other way round, an answer whose person is still
// there is acted on by nobody -- the operator touches no mailbox that has a
// person -- and is taken back here if the removal fails.

// The two answers.
const (
	MailboxArchive = string(gentianov1alpha1.MailboxChoiceArchive)
	MailboxDelete  = string(gentianov1alpha1.MailboxChoiceDelete)
)

// MailboxDecision is one answer, with who gave it.
type MailboxDecision struct {
	Tenant  string
	Address string
	// Choice is MailboxArchive or MailboxDelete.
	Choice string
	// Subject and Name are who decided; RequestID joins the decision to the
	// record of the authority.
	Subject, Name, RequestID string
}

// RemovedMailbox is what an administrator is shown of one decision: whose
// mailbox, what was decided and by whom, and what became of it.
type RemovedMailbox struct {
	// ID names the record, for the request that deletes an archived mailbox.
	ID string `json:"id"`
	// Address is the address the removed person received at.
	Address string `json:"address"`
	// Choice is what was decided: archive or delete.
	Choice string `json:"choice"`
	// State is where it stands: pending, archived, deleted, none (the
	// address never had a mailbox) or failed. Message says more.
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	// RemovedAt is when the person was removed and the choice made; By who
	// made it.
	RemovedAt time.Time `json:"removedAt"`
	By        string    `json:"by,omitempty"`
	// ArchivedAt, SizeBytes and Messages describe an archived mailbox.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	SizeBytes  int64      `json:"sizeBytes,omitempty"`
	Messages   int64      `json:"messages,omitempty"`
	// DeletedAt is when the mailbox, or its archive, was deleted.
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
	// DeletionRequested says the archived mailbox was asked to be deleted
	// and is not gone yet; DeletionBy who asked.
	DeletionRequested bool   `json:"deletionRequested,omitempty"`
	DeletionBy        string `json:"deletionBy,omitempty"`
}

// ErrNotArchived is a request to delete an archived mailbox that is not one.
var ErrNotArchived = errors.New("not an archived mailbox")

// ErrMailboxNotFound is a record this tenant does not have.
var ErrMailboxNotFound = errors.New("no such removed mailbox")

// Mailboxes is where decisions about removed people's mailboxes are written
// down and read back.
type Mailboxes interface {
	// Decide writes a decision down and answers the record's name.
	Decide(ctx context.Context, d MailboxDecision) (string, error)
	// Withdraw takes back a decision whose person was not removed after all.
	Withdraw(ctx context.Context, id string) error
	// List is a tenant's decisions, newest first.
	List(ctx context.Context, tenant string) ([]RemovedMailbox, error)
	// DeleteArchive asks for one archived mailbox of the tenant to be
	// deleted, and answers the address it was.
	DeleteArchive(ctx context.Context, tenant, id string, by MailboxDecision) (string, error)
}

// ClusterMailboxes keeps the decisions as MailboxRemoval objects.
type ClusterMailboxes struct {
	Client client.Client
}

func requester(d MailboxDecision) gentianov1alpha1.MailboxRequester {
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	return gentianov1alpha1.MailboxRequester{Subject: d.Subject, Name: d.Name, RequestID: d.RequestID, At: &now}
}

// Decide creates the record.
func (c *ClusterMailboxes) Decide(ctx context.Context, d MailboxDecision) (string, error) {
	if d.Choice != MailboxArchive && d.Choice != MailboxDelete {
		return "", fmt.Errorf("%q is neither archive nor delete", d.Choice)
	}
	record := &gentianov1alpha1.MailboxRemoval{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: d.Tenant + "-",
			Labels:       map[string]string{"gentianos.io/tenant": d.Tenant},
		},
		Spec: gentianov1alpha1.MailboxRemovalSpec{
			Tenant:      d.Tenant,
			Address:     strings.ToLower(d.Address),
			Mailbox:     gentianov1alpha1.MailboxChoice(d.Choice),
			RequestedBy: requester(d),
		},
	}
	if err := c.Client.Create(ctx, record); err != nil {
		return "", fmt.Errorf("writing the decision down: %w", err)
	}
	return record.Name, nil
}

// Withdraw deletes a record nothing was done for.
func (c *ClusterMailboxes) Withdraw(ctx context.Context, id string) error {
	record := &gentianov1alpha1.MailboxRemoval{ObjectMeta: metav1.ObjectMeta{Name: id}}
	return client.IgnoreNotFound(c.Client.Delete(ctx, record))
}

// List reads a tenant's records.
func (c *ClusterMailboxes) List(ctx context.Context, tenant string) ([]RemovedMailbox, error) {
	var list gentianov1alpha1.MailboxRemovalList
	if err := c.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("reading the removed mailboxes: %w", err)
	}
	out := []RemovedMailbox{}
	for i := range list.Items {
		if r := &list.Items[i]; r.Spec.Tenant == tenant {
			out = append(out, removedMailbox(r))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RemovedAt.After(out[j].RemovedAt) })
	return out, nil
}

func removedMailbox(r *gentianov1alpha1.MailboxRemoval) RemovedMailbox {
	out := RemovedMailbox{
		ID:        r.Name,
		Address:   r.Spec.Address,
		Choice:    string(r.Spec.Mailbox),
		State:     "pending",
		Message:   r.Status.Message,
		RemovedAt: r.CreationTimestamp.Time,
		By:        r.Spec.RequestedBy.Name,
		SizeBytes: r.Status.SizeBytes,
		Messages:  r.Status.Messages,
	}
	switch r.Status.Phase {
	case gentianov1alpha1.MailboxRemovalArchived:
		out.State = "archived"
	case gentianov1alpha1.MailboxRemovalDeleted:
		out.State = "deleted"
	case gentianov1alpha1.MailboxRemovalNoMailbox:
		out.State = "none"
	case gentianov1alpha1.MailboxRemovalFailed:
		out.State = "failed"
	}
	if t := r.Status.ArchivedAt; t != nil {
		out.ArchivedAt = &t.Time
	}
	if t := r.Status.DeletedAt; t != nil {
		out.DeletedAt = &t.Time
	}
	if d := r.Spec.DeleteArchive; d != nil {
		out.DeletionBy = d.Name
		out.DeletionRequested = r.Status.Phase == gentianov1alpha1.MailboxRemovalArchived
	}
	return out
}

// DeleteArchive marks one archived mailbox of the tenant for deletion.
func (c *ClusterMailboxes) DeleteArchive(ctx context.Context, tenant, id string, by MailboxDecision) (string, error) {
	record := &gentianov1alpha1.MailboxRemoval{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: id}, record); err != nil {
		if apierrors.IsNotFound(err) {
			return "", ErrMailboxNotFound
		}
		return "", fmt.Errorf("reading the removed mailbox: %w", err)
	}
	// Another tenant's record does not exist here.
	if record.Spec.Tenant != tenant {
		return "", ErrMailboxNotFound
	}
	if record.Status.Phase != gentianov1alpha1.MailboxRemovalArchived {
		return "", ErrNotArchived
	}
	if record.Spec.DeleteArchive != nil {
		return record.Spec.Address, nil
	}
	asked := requester(by)
	record.Spec.DeleteArchive = &asked
	if err := c.Client.Update(ctx, record); err != nil {
		return "", fmt.Errorf("asking for the archived mailbox to be deleted: %w", err)
	}
	return record.Spec.Address, nil
}

// mailboxOf is the address a person has a mailbox under on the cluster's own
// mail server, or "": the tenant keeps none there, or the person's address
// is not in the domain it keeps them under.
//
// The address is the login where the login is one, and the person's address
// otherwise -- the rule the operator derives a person's mail password by.
func mailboxOf(tenant Tenant, p identity.Person) string {
	if tenant.MailboxDomain == "" {
		return ""
	}
	address := p.Username
	if !strings.Contains(address, "@") {
		address = p.Email
	}
	address = strings.ToLower(strings.TrimSpace(address))
	if !strings.HasSuffix(address, "@"+strings.ToLower(tenant.MailboxDomain)) {
		return ""
	}
	return address
}

// listRemovedMailboxes answers what became of the mailboxes of the people
// removed from this tenant.
func (s *Server) listRemovedMailboxes(w http.ResponseWriter, r *http.Request, _ call) {
	tenant, ok := s.tenantFor(w, r)
	if !ok {
		return
	}
	out := map[string]any{"tenant": tenant.Name, "mailboxDomain": tenant.MailboxDomain, "mailboxes": []RemovedMailbox{}}
	if s.cfg.Mailboxes != nil {
		list, err := s.cfg.Mailboxes.List(r.Context(), tenant.Name)
		if err != nil {
			s.cfg.Log.ErrorContext(r.Context(), "the removed mailboxes could not be read",
				"request_id", reqID(r.Context()), "error", err.Error())
			s.fail(w, r, http.StatusServiceUnavailable, "the removed mailboxes could not be read")
			return
		}
		out["mailboxes"] = list
	}
	s.json(w, http.StatusOK, out)
}

// deleteArchivedMailbox asks for one archived mailbox to be deleted.
func (s *Server) deleteArchivedMailbox(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Mailbox string `json:"mailbox"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if s.cfg.Mailboxes == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "this registrar keeps no record of removed mailboxes")
		return
	}
	if !recordName.MatchString(body.Mailbox) {
		s.fail(w, r, http.StatusBadRequest, "name the archived mailbox: \"mailbox\" is the id the list gives it")
		return
	}
	tenant := r.PathValue("t")
	address, err := s.cfg.Mailboxes.DeleteArchive(r.Context(), tenant, body.Mailbox,
		MailboxDecision{Subject: c.subject, Name: c.name, RequestID: reqID(r.Context())})
	switch {
	case errors.Is(err, ErrMailboxNotFound):
		s.fail(w, r, http.StatusNotFound, "this tenant has no such removed mailbox")
		return
	case errors.Is(err, ErrNotArchived):
		s.fail(w, r, http.StatusConflict, "that mailbox is not archived: there is nothing to delete")
		return
	case err != nil:
		s.cfg.Log.ErrorContext(r.Context(), "the deletion of an archived mailbox could not be asked for",
			"request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable, "the request could not be written down; nothing was changed")
		return
	}
	s.recordIdentityAction(r, c, "delete-archived-mailbox", realm, address)
	s.json(w, http.StatusAccepted, map[string]any{"mailbox": body.Mailbox, "address": address, "deletionRequested": true})
}
