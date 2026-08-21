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

// Package handover records whether this cluster's human write path has been
// proven, and is the only thing that says so.
//
// # Why a record at all
//
// The installer holds a bootstrap token that can write every secret in the
// cluster. It is meant to be destroyed at the end of the install, because a
// credential with every capability, no expiry and no name attached in the audit
// device is not something to leave behind.
//
// After it is destroyed the only way to write a credential is the credential
// manager, which holds no token of its own: it exchanges the caller's Keycloak
// token for a short-lived OpenBao one. That exchange has conditions the rest of
// the install cannot check — the role's bound audience, its bound group claim
// matching what Keycloak actually emits, and the policy attaching — and it
// needs a human in a browser, which no step can perform.
//
// So the revoke step used to check that the *parts* existed: the discovery URL
// is set, the Keycloak client is Ready, the Secret exists, the role exists.
// Every one of those can be true while no token opens anything. Destroying the
// bootstrap token on that evidence is changing the locks and posting the old
// key through the letterbox without trying the new one, and the recovery is
// re-initialising OpenBao.
//
// This package replaces that inference with an observation. The credential
// manager performs the exchange on every request; when one succeeds and carries
// the cluster-admin policy, it records the fact here. Nothing else may write
// the record — it exists to mean "this was seen to work".
//
// # Why the record gates tenant creation too
//
// Not because tenants need the human write path; they do not, the operator
// provisions them with its own identity. It is that re-initialising OpenBao is
// cheap on an empty cluster and catastrophic on one with tenants: every derived
// credential is regenerated, so every tenant database, realm and service
// account stops matching what is deployed against it.
//
// The gate therefore protects the affordability of the recovery, not the
// tenants. That is why it lifts on proof rather than on revocation, and why it
// is on creation only — an existing tenant must stay manageable.
//
// # Why a ConfigMap
//
// One cluster-scoped fact with no reconciliation behind it. A CRD would need a
// controller, an API and a schema migration to carry two booleans and a name.
// Anyone able to forge this ConfigMap already controls the operator's namespace
// and therefore the operator, so it protects nothing that is not already lost.
package handover

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ConfigMapName is the record. Named for what it is about rather than for
	// the component that writes it, because two components read it.
	ConfigMapName = "gentian-handover"

	// KeyWritePathProven is set when an OIDC token exchange has actually
	// succeeded and returned the cluster-admin policy.
	KeyWritePathProven = "writePathProven"
	// KeyProvenAt and KeyProvenBy record when, and for whom. The audit device
	// has the authoritative account; these make the fact legible without it.
	KeyProvenAt = "provenAt"
	KeyProvenBy = "provenBy"

	// KeyBootstrapRevoked is written by the revoke step once the bootstrap
	// token is gone, so an operator can tell a cluster that never finished
	// handover from one that did.
	KeyBootstrapRevoked = "bootstrapCredentialRevoked"
	KeyRevokedAt        = "revokedAt"

	// KeyRecoveryKitExported is written by --export-recovery-kit once a kit
	// has actually left this machine. Read by the revoke step, which — like
	// WritePathProven — refuses to act without it: revoking the bootstrap
	// token is exactly the moment this cluster's only other way in becomes
	// whatever is in that kit, so the file has to exist before that happens,
	// not be taken on faith.
	KeyRecoveryKitExported   = "recoveryKitExported"
	KeyRecoveryKitExportedAt = "recoveryKitExportedAt"
)

// State is what the cluster knows about its own handover.
type State struct {
	// WritePathProven is the whole point: an exchange was seen to succeed.
	WritePathProven bool
	ProvenAt        string
	ProvenBy        string

	// BootstrapRevoked says the installer's token is gone. It follows proof
	// and never precedes it.
	BootstrapRevoked bool
	RevokedAt        string

	// RecoveryKitExported says a break-glass kit has left this machine.
	// Written by --export-recovery-kit, never by anything in this package —
	// the export is a shell command run once, by a human, and this package
	// only reads what it left behind.
	RecoveryKitExported   bool
	RecoveryKitExportedAt string
}

// Read returns the cluster's handover state.
//
// A missing ConfigMap is a valid state, not an error: it is what a cluster
// looks like before anyone has logged in. Callers that cannot distinguish
// "nothing recorded" from "could not read" would gate on infrastructure
// trouble, which is the wrong reason to refuse a tenant.
func Read(ctx context.Context, c client.Client, namespace string) (State, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: namespace}, cm)
	if k8serrors.IsNotFound(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read handover record: %w", err)
	}
	return State{
		WritePathProven:       cm.Data[KeyWritePathProven] == "true",
		ProvenAt:              cm.Data[KeyProvenAt],
		ProvenBy:              cm.Data[KeyProvenBy],
		BootstrapRevoked:      cm.Data[KeyBootstrapRevoked] == "true",
		RevokedAt:             cm.Data[KeyRevokedAt],
		RecoveryKitExported:   cm.Data[KeyRecoveryKitExported] == "true",
		RecoveryKitExportedAt: cm.Data[KeyRecoveryKitExportedAt],
	}, nil
}

// RecordWritePathProven notes that a token exchange succeeded, once.
//
// Idempotent, and deliberately does not refresh the timestamp on later
// successes: the record answers "when was this first seen to work", which is
// the question an operator deciding whether to revoke is asking. Rewriting it
// on every request would also mean a write per API call.
//
// user may be empty — the exchange is the proof, and a role whose user_claim
// did not come back is still a role that opened.
func RecordWritePathProven(ctx context.Context, c client.Client, namespace, user string, now time.Time) error {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: namespace}, cm)
	switch {
	case k8serrors.IsNotFound(err):
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ConfigMapName,
				Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":       "gentian-os",
					"app.kubernetes.io/component":  "handover",
					"app.kubernetes.io/managed-by": "gentian-os",
				},
			},
			Data: provenData(user, now),
		}
		if err := c.Create(ctx, cm); err != nil && !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("create handover record: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read handover record: %w", err)
	}

	if cm.Data[KeyWritePathProven] == "true" {
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for k, v := range provenData(user, now) {
		cm.Data[k] = v
	}
	if err := c.Update(ctx, cm); err != nil {
		return fmt.Errorf("update handover record: %w", err)
	}
	return nil
}

func provenData(user string, now time.Time) map[string]string {
	d := map[string]string{
		KeyWritePathProven: "true",
		KeyProvenAt:        now.UTC().Format(time.RFC3339),
	}
	if user != "" {
		d[KeyProvenBy] = user
	}
	return d
}
