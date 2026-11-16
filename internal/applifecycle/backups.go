/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// What the cluster knows about a tenant's backups, for the console to render
// through the director.
//
// Reads only. A policy is declared state and is changed by committing it to
// git, like a resource plan; this answers what the cluster currently has,
// which is a different question and the one a screen opens with. The
// inheritance — a tenant's policy over the cluster's, and the cluster's over
// the platform's own storage — is resolved by the BackupPolicy reconciler and
// reported on the CR's status, so it is read here rather than recomputed.

// BackupSummary is one export as the API presents it.
type BackupSummary struct {
	Name        string `json:"name"`
	Tenant      string `json:"tenant"`
	Phase       string `json:"phase"`
	CreatedAt   string `json:"createdAt"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
	// BundleBucket and BundlePrefix say where the bundle was written, empty
	// until it has been.
	BundleBucket string `json:"bundleBucket,omitempty"`
	BundlePrefix string `json:"bundlePrefix,omitempty"`
	// EncryptionMode is whose key the bundle is readable with.
	EncryptionMode string `json:"encryptionMode,omitempty"`
	// PlatformReadable reports whether the platform can open this bundle. A
	// tenant that brought its own key gets bundles nobody here can read, and
	// the screen has to say so before someone asks for help restoring one.
	PlatformReadable bool `json:"platformReadable"`
	// Quiesced names the apps that were paused for consistency.
	Quiesced []string        `json:"quiesced"`
	Apps     []BackupAppInfo `json:"apps"`
	Message  string          `json:"message,omitempty"`
	// Schedule names the schedule that took this export, empty for one a
	// person started.
	Schedule string `json:"schedule,omitempty"`
	// Reason says why the platform took an export nobody asked for: "move",
	// one taken before an app moved to another build. A screen lists those
	// apart from backups.
	Reason string `json:"reason,omitempty"`
}

// BackupAppInfo is one app's part of an export.
type BackupAppInfo struct {
	Name         string   `json:"name"`
	Phase        string   `json:"phase"`
	Stores       []string `json:"stores"`
	ChartVersion string   `json:"chartVersion,omitempty"`
	QuiesceStart string   `json:"quiesceStart,omitempty"`
	QuiesceEnd   string   `json:"quiesceEnd,omitempty"`
	Message      string   `json:"message,omitempty"`
}

// BackupPolicyResult is one scope's policy: what it states, and what actually
// applies once inheritance has been resolved.
type BackupPolicyResult struct {
	Scope  string `json:"scope"`
	Tenant string `json:"tenant,omitempty"`
	// Configured is false when this scope states nothing and inherits
	// everything. An empty policy and an absent one are the same answer here,
	// and the screen renders "inherited" rather than a form full of blanks.
	Configured  bool                    `json:"configured"`
	Destination BackupDestinationResult `json:"destination"`
	Encryption  BackupEncryptionResult  `json:"encryption"`
	// AllowTenantOverride is read from the cluster policy only.
	AllowTenantOverride bool `json:"allowTenantOverride"`
	// The effective values, as the reconciler resolved them.
	EffectiveEndpoint   string   `json:"effectiveEndpoint,omitempty"`
	EffectiveBucket     string   `json:"effectiveBucket,omitempty"`
	EffectiveRecipients []string `json:"effectiveRecipients"`
	// CredentialRequirement names what has to be supplied before bundles can
	// be written to this destination, and whether it has been.
	CredentialRequirement string `json:"credentialRequirement,omitempty"`
	CredentialSatisfied   bool   `json:"credentialSatisfied"`
	Message               string `json:"message,omitempty"`
}

// BackupDestinationResult is where bundles are written.
type BackupDestinationResult struct {
	Endpoint        string `json:"endpoint,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Region          string `json:"region,omitempty"`
	AddressingStyle string `json:"addressingStyle,omitempty"`
}

// BackupEncryptionResult names the keys bundles are encrypted to. An empty
// recipient list means the platform's own key, which is what makes a restore
// something the platform can help with.
type BackupEncryptionResult struct {
	Mode       string   `json:"mode"`
	Recipients []string `json:"recipients"`
}

// Backups lists a tenant's exports, newest first.
func (s *Service) Backups(ctx context.Context, tenantName string) ([]BackupSummary, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return nil, err
	}
	var list gentianov1alpha1.TenantExportList
	if err := s.client.List(ctx, &list, client.InNamespace(layout.Tenant(tenantName))); err != nil {
		return nil, fmt.Errorf("list exports for %s: %w", tenantName, err)
	}
	out := make([]BackupSummary, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, backupSummary(&list.Items[i], tenantName))
	}
	// Newest first: a backup screen is opened to see the last one.
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// Backup reports one export.
func (s *Service) Backup(ctx context.Context, tenantName, name string) (*BackupSummary, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return nil, err
	}
	var export gentianov1alpha1.TenantExport
	key := types.NamespacedName{Name: name, Namespace: layout.Tenant(tenantName)}
	if err := s.client.Get(ctx, key, &export); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("backup %q not found for tenant %q", name, tenantName)
		}
		return nil, err
	}
	summary := backupSummary(&export, tenantName)
	return &summary, nil
}

// BackupPolicy reports one scope's policy. An absent policy is not an error:
// it is a scope that states nothing and inherits, which is the ordinary case
// and what the screen has to render.
func (s *Service) BackupPolicy(ctx context.Context, scope, tenantName string) (*BackupPolicyResult, error) {
	name := backup.ClusterPolicyName
	if scope == "tenant" {
		if _, err := s.getTenant(ctx, tenantName); err != nil {
			return nil, err
		}
		name = tenantName
	}

	result := &BackupPolicyResult{Scope: scope, Tenant: tenantName, EffectiveRecipients: []string{}}
	var policy gentianov1alpha1.BackupPolicy
	err := s.client.Get(ctx, types.NamespacedName{Name: name}, &policy)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing of its own. The cluster's effective values still answer for
		// a tenant, so they are read from the cluster policy below.
		result.AllowTenantOverride = true
	case err != nil:
		return nil, err
	default:
		if policy.Spec.Scope != scope || (scope == "tenant" && policy.Spec.Tenant != tenantName) {
			// A name collision rather than this tenant's policy.
			result.AllowTenantOverride = true
			break
		}
		result.Configured = true
		result.AllowTenantOverride = policy.Spec.OverrideAllowed()
		if d := policy.Spec.Destination; d != nil {
			result.Destination = BackupDestinationResult{Endpoint: d.Endpoint, Bucket: d.Bucket, Region: d.Region,
				AddressingStyle: string(d.AddressingStyle)}
		}
		result.Encryption = encryptionResult(policy.Spec.Encryption)
		result.EffectiveEndpoint = policy.Status.EffectiveEndpoint
		result.EffectiveBucket = policy.Status.EffectiveBucket
		if policy.Status.EffectiveRecipients != nil {
			result.EffectiveRecipients = policy.Status.EffectiveRecipients
		}
		result.CredentialRequirement = policy.Status.CredentialRequirement
		result.CredentialSatisfied = policy.Status.CredentialSatisfied
		result.Message = conditionMessage(policy.Status.Conditions)
	}

	// A tenant with no policy of its own still runs under one. Reading the
	// cluster's effective values is what lets the screen say what applies
	// rather than showing an empty form and leaving the reader to guess.
	if scope == "tenant" && !result.Configured {
		var cluster gentianov1alpha1.BackupPolicy
		if err := s.client.Get(ctx, types.NamespacedName{Name: backup.ClusterPolicyName}, &cluster); err == nil {
			result.EffectiveEndpoint = cluster.Status.EffectiveEndpoint
			result.EffectiveBucket = cluster.Status.EffectiveBucket
			if cluster.Status.EffectiveRecipients != nil {
				result.EffectiveRecipients = cluster.Status.EffectiveRecipients
			}
			result.AllowTenantOverride = cluster.Spec.OverrideAllowed()
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return result, nil
}

func backupSummary(e *gentianov1alpha1.TenantExport, tenantName string) BackupSummary {
	out := BackupSummary{
		Name:      e.Name,
		Tenant:    tenantName,
		Phase:     string(e.Status.Phase),
		CreatedAt: e.CreationTimestamp.UTC().Format(metav1.RFC3339Micro),
		Quiesced:  e.Status.Quiesced,
		Apps:      []BackupAppInfo{},
		Message:   conditionMessage(e.Status.Conditions),
		Schedule:  e.Labels[gentianov1alpha1.TenantExportScheduleLabel],
		Reason:    e.Labels[gentianov1alpha1.TenantExportReasonLabel],
	}
	if out.Quiesced == nil {
		out.Quiesced = []string{}
	}
	if e.Status.StartedAt != nil {
		out.StartedAt = e.Status.StartedAt.UTC().Format(metav1.RFC3339Micro)
	}
	if e.Status.CompletedAt != nil {
		out.CompletedAt = e.Status.CompletedAt.UTC().Format(metav1.RFC3339Micro)
	}
	if b := e.Status.Bundle; b != nil {
		out.BundleBucket, out.BundlePrefix = b.Bucket, b.Prefix
	}
	if enc := e.Status.Encryption; enc != nil {
		out.EncryptionMode = string(enc.Mode)
		// The platform can open a bundle it encrypted to its own key and no
		// other. Recipients of its own mean a key only the tenant holds.
		out.PlatformReadable = len(enc.Recipients) == 0
	}
	for i := range e.Status.Apps {
		a := &e.Status.Apps[i]
		info := BackupAppInfo{
			Name: a.Name, Phase: string(a.Phase), Stores: a.Stores,
			ChartVersion: a.ChartVersion, Message: a.Message,
		}
		if info.Stores == nil {
			info.Stores = []string{}
		}
		if a.QuiesceStart != nil {
			info.QuiesceStart = a.QuiesceStart.UTC().Format(metav1.RFC3339Micro)
		}
		if a.QuiesceEnd != nil {
			info.QuiesceEnd = a.QuiesceEnd.UTC().Format(metav1.RFC3339Micro)
		}
		out.Apps = append(out.Apps, info)
	}
	return out
}

// encryptionResult reads a policy's encryption. No recipients of its own
// means the platform's key, which is what makes a restore something the
// platform can help with; recipients mean a key only the tenant holds.
func encryptionResult(e *gentianov1alpha1.BackupEncryption) BackupEncryptionResult {
	out := BackupEncryptionResult{Mode: "platform", Recipients: []string{}}
	if e.IsSet() {
		out.Mode = "own"
		out.Recipients = e.Recipients
	}
	return out
}

// conditionMessage is the most useful sentence on a status: the first
// condition that is not True, or the Ready one's message.
func conditionMessage(conditions []metav1.Condition) string {
	for i := range conditions {
		if conditions[i].Status != metav1.ConditionTrue {
			return conditions[i].Message
		}
	}
	for i := range conditions {
		if conditions[i].Type == "Ready" {
			return conditions[i].Message
		}
	}
	return ""
}

// ── Actions ──────────────────────────────────────────────────────────────────
//
