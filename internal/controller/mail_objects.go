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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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
