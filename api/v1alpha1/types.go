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

// AppDeploymentMode controls how an application is deployed relative to tenants.
// "dedicated" provisions a separate deployment per tenant in the tenant namespace.
// "shared" provisions a single shared deployment in the platform-kernel namespace
// with per-tenant IAM brokering via the shared-apps Keycloak realm. All tenants
// using "shared" for the same app profile share the same Helm release and OIDC
// client. Only one App claim is created (in platform-kernel); it is not cleaned
// up on individual tenant removal.
// +kubebuilder:validation:Enum=dedicated;shared
type AppDeploymentMode string

const (
	// AppDeploymentModeDedicated provisions a separate app instance per tenant.
	AppDeploymentModeDedicated AppDeploymentMode = "dedicated"
	// AppDeploymentModeShared provisions a single shared app instance in the
	// platform-kernel namespace. All tenants using this mode share the same
	// deployment and OIDC client; per-tenant IAM brokering is set up via the
	// shared-apps Keycloak realm.
	AppDeploymentModeShared AppDeploymentMode = "shared"
)

// CacheEngine specifies which caching backend a kernel requirement uses.
// +kubebuilder:validation:Enum=redis;memcached
type CacheEngine string

const (
	CacheEngineRedis     CacheEngine = "redis"
	CacheEngineMemcached CacheEngine = "memcached"
)

// DeploymentMethod determines how the orchestrator delivers the app Helm release.
// +kubebuilder:validation:Enum=argocd;crossplane
type DeploymentMethod string

const (
	// DeploymentMethodArgoCD uses an ArgoCD Application CR for kernel-layer services
	// that are managed directly by the cache or identity reconcilers.
	DeploymentMethodArgoCD DeploymentMethod = "argocd"
	// DeploymentMethodCrossplane uses a Crossplane App claim. The claim drives an
	// App Composition that emits an ExternalSecret and a provider-helm Release.
	DeploymentMethodCrossplane DeploymentMethod = "crossplane"
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
