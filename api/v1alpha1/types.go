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

package v1alpha1

// IsolationMode defines how a tenant's workloads are isolated.
// +kubebuilder:validation:Enum=namespace;vcluster
type IsolationMode string

const (
	// IsolationModeNamespace uses a dedicated Kubernetes namespace per tenant.
	IsolationModeNamespace IsolationMode = "namespace"
	// IsolationModeVcluster uses a vCluster per tenant for stronger isolation.
	IsolationModeVcluster IsolationMode = "vcluster"
)

// MailMode defines how a tenant's mail is handled.
// +kubebuilder:validation:Enum=selfhosted;external;transport-only;disabled
type MailMode string

const (
	// MailModeSelfhosted registers the tenant in the SHARED kernel Postfix and
	// Dovecot — one stack per cluster, tenant-scoped by domain and maildir path.
	// It does not deploy anything per tenant; whether Dovecot exists at all is
	// the cluster's mail.serviceMode, not this field. See docs/design/mail.md §8.
	MailModeSelfhosted MailMode = "selfhosted"
	// MailModeExternal relays through the tenant's own provider, whose credentials
	// it supplies as spec.mail.smtpCredentialsSecret. Required for this mode.
	MailModeExternal MailMode = "external"
	// MailModeTransportOnly registers the tenant in the shared Postfix relay for
	// outbound only — no Dovecot registration, so no mailbox and no IMAP.
	MailModeTransportOnly MailMode = "transport-only"
	// MailModeDisabled provisions no mail for the tenant at all.
	MailModeDisabled MailMode = "disabled"
)

// DeletionPolicy controls what happens to tenant resources on CR deletion.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain revokes access credentials but keeps all data.
	// This is the safe default for compliance and accidental-deletion recovery.
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete removes all tenant resources including databases,
	// storage buckets, and mailboxes. Intended for dev/test.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// DatabaseEngine specifies which database engine a kernel requirement uses.
// +kubebuilder:validation:Enum=postgresql;mariadb
type DatabaseEngine string

const (
	DatabaseEnginePostgreSQL DatabaseEngine = "postgresql"
	DatabaseEngineMariaDB    DatabaseEngine = "mariadb"
)

// CacheEngine specifies which caching backend a kernel requirement uses.
// +kubebuilder:validation:Enum=redis;memcached
type CacheEngine string

const (
	CacheEngineRedis     CacheEngine = "redis"
	CacheEngineMemcached CacheEngine = "memcached"
)

// DeploymentMethod determines how the orchestrator delivers the app.
//
// There used to be a third value, "argocd", from when the operator created a
// per-app Argo CD Application itself. That path is gone — ensureAppDeployment
// creates no Applications — and no profile in the catalogue declared it. What
// kept it alive was one branch treating any non-crossplane value as "the
// operator owns the OIDC client", and a dozen test fixtures using it as an
// arbitrary value, which meant the suite exercised a configuration no real
// profile had.
//
// +kubebuilder:validation:Enum=crossplane;api
type DeploymentMethod string

const (
	// DeploymentMethodCrossplane uses a Crossplane App claim. The claim drives an
	// App Composition that emits an ExternalSecret and a provider-helm Release.
	DeploymentMethodCrossplane DeploymentMethod = "crossplane"
	// DeploymentMethodAPI marks an ApiProfile: a catalogue entry that runs no
	// workload pods. The orchestrator provisions no Helm release or App claim;
	// runtime traffic is served by an external service (see spec.apiIntegration).
	DeploymentMethodAPI DeploymentMethod = "api"
)

// BackupQuiesceMode selects how an app's writes are paused while it is captured.
// +kubebuilder:validation:Enum=none;scaleDown;command
type BackupQuiesceMode string

const (
	// BackupQuiesceNone captures the app while it keeps serving. Only safe for
	// an app whose stores hold no references to each other, since nothing keeps
	// two of them mutually consistent.
	BackupQuiesceNone BackupQuiesceMode = "none"
	// BackupQuiesceScaleDown scales the app's workloads to zero for the
	// duration of the capture. The default: it works for every app, at the cost
	// of the longest pause.
	BackupQuiesceScaleDown BackupQuiesceMode = "scaleDown"
	// BackupQuiesceCommand runs the profile's own maintenance-mode commands,
	// which usually pauses writes without taking the app offline.
	BackupQuiesceCommand BackupQuiesceMode = "command"
)

// BackupConsistency selects the window an app's stores must be captured within.
// +kubebuilder:validation:Enum=app;perStore
type BackupConsistency string

const (
	// BackupConsistencyApp captures every store of the app inside a single
	// quiesce window, because its database, buckets and volumes reference each
	// other. This is the boundary that matters; two different apps share no
	// transactional state, so skew between them is harmless.
	BackupConsistencyApp BackupConsistency = "app"
	// BackupConsistencyPerStore lets each store be captured independently, for
	// an app whose stores hold no references to each other.
	BackupConsistencyPerStore BackupConsistency = "perStore"
)

// TenantPhase represents the overall lifecycle phase of a Tenant.
// +kubebuilder:validation:Enum=Provisioning;Ready;Degraded;Deleting
type TenantPhase string

const (
	TenantPhaseProvisioning TenantPhase = "Provisioning"
	TenantPhaseReady        TenantPhase = "Ready"
	TenantPhaseDegraded     TenantPhase = "Degraded"
	TenantPhaseDeleting     TenantPhase = "Deleting"
)

// IntegrationBindingState represents the overall state of a binding.
// +kubebuilder:validation:Enum=Pending;Ready;Failed
type IntegrationBindingState string

const (
	IntegrationBindingStatePending IntegrationBindingState = "Pending"
	IntegrationBindingStateReady   IntegrationBindingState = "Ready"
	IntegrationBindingStateFailed  IntegrationBindingState = "Failed"
)
