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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Where each object the operator writes for mail lives.
//
// The rule is one sentence: an object goes where its reader reads it. A pod
// mounts a ConfigMap or a Secret from its own namespace and no other, a Job
// reads a Secret reference from its own, and the operator reads a Service's
// address where the Service is. Every fault this file exists for was an
// object written by function name -- "the mail DMZ", "the services
// namespace" -- into a namespace its reader was not in, with an optional
// mount or a tolerated lookup hiding the miss. mail_objects_test.go holds
// each of these to the chart or the Job that reads it.
var (
	// Postfix: its maps and its DKIM tables are mounted by its pod.
	postfixNamespace = mailNamespace
	// Dovecot: the passwd-files and the per-realm token settings are mounted
	// by its pod.
	dovecotNamespace = mailNamespace
	// The seed every mail password of a tenant is derived from. Only the
	// operator reads it; it stays beside the hashes it produces, in a
	// namespace nothing outside the cluster reaches.
	mailSeedNamespace = mailNamespace
	// The password a realm submits mail with, in the clear: read by the Job
	// that configures the realm and by the realm the tenant's Composition
	// declares, both beside Keycloak.
	mailSubmissionNamespace = identityNamespace
	// The mail edge: the proxy that faces the internet, the load-balancer
	// Service in front of it whose address the DNS records carry, and those
	// records. Nothing here is mounted by a mail server.
	mailEdgeNamespace = mailDMZNamespace
)

// dovecotAppPasswordsSecret holds the passwd-files Dovecot verifies an app
// password and a submission credential against.
const dovecotAppPasswordsSecret = "dovecot-app-passwords"

// misplacedMailObjectsNamespace is where the operator used to write the
// Secrets of this file: the namespace of the Gateway and its routes.
func misplacedMailObjectsNamespace() string { return defaultServicesNamespace() }

// postfixAcceptsDomain reports whether the map Postfix mounts names the
// domain, which is the difference between a registered tenant and one whose
// mail is delivered.
func (r *TenantReconciler) postfixAcceptsDomain(ctx context.Context, domain string) (bool, error) {
	if domain == "" {
		return false, nil
	}
	maps := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: postfixVirtualMailboxMapsConfigMap, Namespace: postfixNamespace}, maps)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(maps.Data[postfixVirtualMailboxDomainsKey], "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.EqualFold(fields[0], domain) {
			return true, nil
		}
	}
	return false, nil
}

// adoptMisplacedMailSeed moves a tenant's seed from the namespace it used to
// be written to into the mail namespace, and returns it. Empty when there is
// none to move.
func (r *TenantReconciler) adoptMisplacedMailSeed(ctx context.Context, name string) ([]byte, error) {
	from := misplacedMailObjectsNamespace()
	if from == mailSeedNamespace {
		return nil, nil
	}
	moved, err := r.moveSecret(ctx, name, from, mailSeedNamespace)
	if err != nil || moved == nil {
		return nil, err
	}
	return moved.Data["seed"], nil
}

// moveSecret copies a Secret the operator wrote into the namespace it
// belongs in and removes the original. The copy is made first, and an
// existing Secret at the destination wins: it is the one already in use.
func (r *TenantReconciler) moveSecret(ctx context.Context, name, from, to string) (*corev1.Secret, error) {
	old := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: from}, old); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if old.Labels[managedByLabel] != managedByValue {
		return nil, nil
	}
	current := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: to}, current)
	switch {
	case errors.IsNotFound(err):
		current = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: to, Labels: old.Labels},
			Type:       old.Type,
			Data:       old.Data,
		}
		if err := r.Create(ctx, current); err != nil {
			return nil, fmt.Errorf("move Secret %s from %s to %s: %w", name, from, to, err)
		}
	case err != nil:
		return nil, err
	}
	if err := r.Delete(ctx, old); client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("remove Secret %s/%s after moving it: %w", from, name, err)
	}
	return current, nil
}

// retireMisplacedMailObjects removes what earlier versions wrote where no
// reader was, once, when the operator starts.
//
//   - Postfix's maps, in the mail DMZ: derived from the registry, so the copy
//     in the mail namespace is rebuilt and this one is only deleted.
//   - Dovecot's passwd-files, in the edge namespace: every line is written
//     again by the reconcile of the tenant it belongs to, verified against
//     the credential, so this one is only deleted too. Dovecot never read it.
//   - Each tenant's seed and submission password, in the edge namespace:
//     moved, because the first is not derivable and the second is in use by
//     a realm until the operator has written it again.
//
// Each step is independent and a failure is returned after the others have
// run: a Secret that could not be deleted must not keep a seed from moving.
func (r *TenantReconciler) retireMisplacedMailObjects(ctx context.Context) error {
	var failures []string
	note := func(err error) {
		if err != nil {
			failures = append(failures, err.Error())
		}
	}

	if mailEdgeNamespace != postfixNamespace {
		stale := &corev1.ConfigMap{}
		err := r.Get(ctx, types.NamespacedName{Name: postfixVirtualMailboxMapsConfigMap, Namespace: mailEdgeNamespace}, stale)
		if err == nil && stale.Labels[managedByLabel] == managedByValue {
			log.FromContext(ctx).Info("removing Postfix's maps from the namespace Postfix does not run in",
				"namespace", mailEdgeNamespace, "name", stale.Name)
			note(client.IgnoreNotFound(r.Delete(ctx, stale)))
		} else {
			note(client.IgnoreNotFound(err))
		}
	}

	from := misplacedMailObjectsNamespace()
	if from != dovecotNamespace {
		stale := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: dovecotAppPasswordsSecret, Namespace: from}, stale)
		if err == nil && stale.Labels[managedByLabel] == managedByValue {
			log.FromContext(ctx).Info("removing Dovecot's passwd-files from the namespace Dovecot does not run in",
				"namespace", from, "name", stale.Name)
			note(client.IgnoreNotFound(r.Delete(ctx, stale)))
		} else {
			note(client.IgnoreNotFound(err))
		}
	}

	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(from), client.MatchingLabels{managedByLabel: managedByValue}); err != nil {
		note(err)
	}
	for i := range secrets.Items {
		name := secrets.Items[i].Name
		var to string
		switch {
		case strings.HasPrefix(name, mailAppPasswordSeedName+"-"):
			to = mailSeedNamespace
		case strings.HasPrefix(name, mailSubmissionSecretPrefix):
			to = mailSubmissionNamespace
		default:
			continue
		}
		if to == from {
			continue
		}
		log.FromContext(ctx).Info("moving a mail credential out of the edge namespace", "name", name, "from", from, "to", to)
		_, err := r.moveSecret(ctx, name, from, to)
		note(err)
	}

	if len(failures) > 0 {
		return fmt.Errorf("retire misplaced mail objects: %s", strings.Join(failures, "; "))
	}
	return nil
}
