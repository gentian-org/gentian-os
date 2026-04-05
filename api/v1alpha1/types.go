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
	// MailModeSelfhosted deploys a full Postfix+Dovecot stack per tenant.
	MailModeSelfhosted MailMode = "selfhosted"
	// MailModeExternal routes mail to an external provider via SMTP relay.
	MailModeExternal MailMode = "external"
	// MailModeTransportOnly delivers outbound mail only; no IMAP storage.
	MailModeTransportOnly MailMode = "transport-only"
	// MailModeDisabled disables all mail functionality for the tenant.
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
	// storage buckets, mailboxes, and LDAP entries. Intended for dev/test.
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

// DeploymentMethod determines how the orchestrator delivers the app Helm release.
// +kubebuilder:validation:Enum=argocd;tofu-controller
type DeploymentMethod string

const (
	// DeploymentMethodArgoCD uses an ArgoCD Application CR (Pattern A — existingSecret).
	DeploymentMethodArgoCD DeploymentMethod = "argocd"
	// DeploymentMethodTofuController uses a Tofu Controller Workspace with set_sensitive
	// for charts that do not support existingSecret references (Pattern B).
	DeploymentMethodTofuController DeploymentMethod = "tofu-controller"
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
