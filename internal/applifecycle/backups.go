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
	Configured      bool                    `json:"configured"`
	Destination     BackupDestinationResult `json:"destination"`
	Schedule        string                  `json:"schedule,omitempty"`
	SuspendSchedule bool                    `json:"suspendSchedule"`
	Retention       BackupRetentionResult   `json:"retention"`
	Encryption      BackupEncryptionResult  `json:"encryption"`
	// AllowTenantOverride is read from the cluster policy only.
	AllowTenantOverride bool `json:"allowTenantOverride"`
	// The effective values, as the reconciler resolved them.
	EffectiveEndpoint   string   `json:"effectiveEndpoint,omitempty"`
	EffectiveBucket     string   `json:"effectiveBucket,omitempty"`
	EffectiveSchedule   string   `json:"effectiveSchedule,omitempty"`
	EffectiveRecipients []string `json:"effectiveRecipients"`
	// CredentialRequirement names what has to be supplied before bundles can
	// be written to this destination, and whether it has been.
	CredentialRequirement string `json:"credentialRequirement,omitempty"`
	CredentialSatisfied   bool   `json:"credentialSatisfied"`
	Message               string `json:"message,omitempty"`
}

// BackupDestinationResult is where bundles are written.
type BackupDestinationResult struct {
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Region   string `json:"region,omitempty"`
}

// BackupRetentionResult is which bundles survive.
type BackupRetentionResult struct {
	KeepLast    int32 `json:"keepLast"`
	KeepDaily   int32 `json:"keepDaily"`
	KeepWeekly  int32 `json:"keepWeekly"`
	KeepMonthly int32 `json:"keepMonthly"`
	KeepYearly  int32 `json:"keepYearly"`
}

// BackupEncryptionResult names the keys bundles are encrypted to. An empty
// recipient list means the platform's own key, which is what makes a restore
// something the platform can help with.
type BackupEncryptionResult struct {
	Mode       string   `json:"mode"`
	Recipients []string `json:"recipients"`
}

// BackupScheduleResult is one schedule as the API presents it.
type BackupScheduleResult struct {
	Name      string `json:"name"`
	Tenant    string `json:"tenant"`
	Schedule  string `json:"schedule"`
	Suspended bool   `json:"suspended"`
	// Managed is true for the schedule the BackupPolicy reconciler owns.
	// Editing that one is reverted on the next reconcile, so the screen shows
	// it as derived rather than offering a form that will not hold.
	Managed            bool                   `json:"managed"`
	Retention          BackupRetentionResult  `json:"retention"`
	Encryption         BackupEncryptionResult `json:"encryption"`
	LastScheduleTime   string                 `json:"lastScheduleTime,omitempty"`
	LastSuccessfulTime string                 `json:"lastSuccessfulTime,omitempty"`
	NextScheduleTime   string                 `json:"nextScheduleTime,omitempty"`
	Message            string                 `json:"message,omitempty"`
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
		result.Schedule = policy.Spec.Schedule
		result.SuspendSchedule = policy.Spec.SuspendSchedule
		result.AllowTenantOverride = policy.Spec.OverrideAllowed()
		if d := policy.Spec.Destination; d != nil {
			result.Destination = BackupDestinationResult{Endpoint: d.Endpoint, Bucket: d.Bucket, Region: d.Region}
		}
		result.Retention = retentionResult(policy.Spec.Retention)
		result.Encryption = encryptionResult(policy.Spec.Encryption)
		result.EffectiveEndpoint = policy.Status.EffectiveEndpoint
		result.EffectiveBucket = policy.Status.EffectiveBucket
		result.EffectiveSchedule = policy.Status.EffectiveSchedule
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
			result.EffectiveSchedule = cluster.Status.EffectiveSchedule
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

// BackupSchedules lists the schedules for one tenant, or for every tenant.
func (s *Service) BackupSchedules(ctx context.Context, tenantName string, allTenants bool) ([]BackupScheduleResult, error) {
	opts := []client.ListOption{}
	if !allTenants {
		if _, err := s.getTenant(ctx, tenantName); err != nil {
			return nil, err
		}
		opts = append(opts, client.InNamespace(layout.Tenant(tenantName)))
	}
	var list gentianov1alpha1.TenantExportScheduleList
	if err := s.client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	out := make([]BackupScheduleResult, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, scheduleResult(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
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

func scheduleResult(s *gentianov1alpha1.TenantExportSchedule) BackupScheduleResult {
	out := BackupScheduleResult{
		Name: s.Name,
		// A schedule names no tenant: it is in the tenant's namespace, which
		// is what says whose it is.
		Tenant:    tenantFromNamespace(s.Namespace),
		Schedule:  s.Spec.Schedule,
		Suspended: s.Spec.Suspend,
		// The reconciler owns exactly one schedule per tenant and restates it
		// from the policy on every pass.
		Managed:    s.Name == backup.ManagedScheduleName,
		Retention:  retentionResult(s.Spec.Retention),
		Encryption: exportEncryptionResult(s.Spec.Encryption),
		Message:    conditionMessage(s.Status.Conditions),
	}
	if t := s.Status.LastScheduleTime; t != nil {
		out.LastScheduleTime = t.UTC().Format(metav1.RFC3339Micro)
	}
	if t := s.Status.LastSuccessfulTime; t != nil {
		out.LastSuccessfulTime = t.UTC().Format(metav1.RFC3339Micro)
	}
	if t := s.Status.NextScheduleTime; t != nil {
		out.NextScheduleTime = t.UTC().Format(metav1.RFC3339Micro)
	}
	return out
}

func retentionResult(r *gentianov1alpha1.BackupRetention) BackupRetentionResult {
	if r == nil {
		return BackupRetentionResult{}
	}
	return BackupRetentionResult{
		KeepLast: r.KeepLast, KeepDaily: r.KeepDaily, KeepWeekly: r.KeepWeekly,
		KeepMonthly: r.KeepMonthly, KeepYearly: r.KeepYearly,
	}
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

// exportEncryptionResult reads a schedule's, which is the export type rather
// than the policy type — the same question, asked of the object that carries
// what each run will do.
func exportEncryptionResult(e *gentianov1alpha1.ExportEncryption) BackupEncryptionResult {
	out := BackupEncryptionResult{Mode: "platform", Recipients: []string{}}
	if e != nil && len(e.Recipients) > 0 {
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

func tenantFromNamespace(ns string) string {
	const prefix = "tenant-"
	if len(ns) > len(prefix) && ns[:len(prefix)] == prefix {
		return ns[len(prefix):]
	}
	return ""
}
