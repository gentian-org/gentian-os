package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AppProfileSpec defines the desired state of AppProfile.
type AppProfileSpec struct {
	// DisplayName is a human-readable name for the application.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description is an optional human-readable description.
	// +optional
	Description string `json:"description,omitempty"`

	// KernelRequirements declares which kernel services this app requires.
	// +optional
	KernelRequirements *KernelRequirements `json:"kernelRequirements,omitempty"`

	// Provides lists the integration contracts this app can act as a provider for.
	// +optional
	Provides []ContractRef `json:"provides,omitempty"`

	// OptionalIntegrations lists contracts this app can consume when available.
	// +optional
	OptionalIntegrations []IntegrationRef `json:"optionalIntegrations,omitempty"`

	// Chart references the upstream Helm chart for this app.
	// +kubebuilder:validation:Required
	Chart ChartRef `json:"chart"`

	// ValueMapping is the schema-based mapping of kernel-provided values to
	// Helm value keys. Validated at admission time.
	// +optional
	ValueMapping *ValueMapping `json:"valueMapping,omitempty"`

	// AppSecrets declares app-internal secrets (admin passwords, session keys,
	// cluster tokens) that don't correspond to any kernel function. The
	// orchestrator generates these deterministically (HMAC-SHA256 from master
	// password + tenant + app + secret name), stores them in OpenBao, and syncs
	// them via ExternalSecret.
	// +optional
	AppSecrets []AppSecret `json:"appSecrets,omitempty"`

	// ExtraValues provides an escape hatch for non-standard values that don't
	// fit the typed valueMapping schema. Merged into the rendered Helm values.
	// Must not contain secrets — use appSecrets for secret values.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraValues *runtime.RawExtension `json:"extraValues,omitempty"`

	// DeploymentMethod controls how the orchestrator deploys this app.
	// Defaults to crossplane. Use argocd for kernel-layer services managed
	// directly by the cache or identity reconcilers.
	// +optional
	// +kubebuilder:default=crossplane
	DeploymentMethod DeploymentMethod `json:"deploymentMethod,omitempty"`

	// CompositionRef overrides the Crossplane Composition used to deploy this
	// app. When empty the XRD default (app-default) applies. Set to the name
	// of a purpose-built composition (e.g. "app-element", "app-ox") for
	// profiles that require deploying multiple Helm Releases.
	// +optional
	CompositionRef string `json:"compositionRef,omitempty"`

	// Ingress declares the HTTP routing configuration for this app.
	// When set, the orchestrator creates a Kubernetes Ingress resource and a
	// cert-manager Certificate CR for TLS.
	// +optional
	Ingress *IngressSpec `json:"ingress,omitempty"`
}

// IngressSpec declares how the orchestrator should expose this app via HTTP(S).
type IngressSpec struct {
	// ServiceName is the Kubernetes Service name that the Helm chart creates.
	// Defaults to the AppProfile name (chart name) when not set.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// ServicePort is the port on the Service to route traffic to.
	// +optional
	// +kubebuilder:default=80
	ServicePort int32 `json:"servicePort,omitempty"`

	// SubDomain is the subdomain prefix prepended to the tenant domain to form the
	// Ingress host, e.g. "files" yields "files.{tenant-domain}".
	// Defaults to the app profile name (chart name) when not set.
	// +optional
	SubDomain string `json:"subDomain,omitempty"`

	// IngressClassName is the Kubernetes IngressClass to use.
	// Defaults to "nginx" when not set.
	// +optional
	// +kubebuilder:default=nginx
	IngressClassName string `json:"ingressClassName,omitempty"`

	// TLSEnabled enables TLS via a cert-manager Certificate CR.
	// Defaults to true.
	// +optional
	// +kubebuilder:default=true
	TLSEnabled bool `json:"tlsEnabled,omitempty"`

	// ClusterIssuer is the cert-manager ClusterIssuer to use for TLS.
	// Defaults to "letsencrypt-http01" (HTTP-01 per-host) when not set.
	// See docs/architecture.md §2.5 for the kernel-vs-vanity domain
	// model and the wildcard fallback.
	// +optional
	// +kubebuilder:default=letsencrypt-http01
	ClusterIssuer string `json:"clusterIssuer,omitempty"`

	// Annotations are merged into the Ingress object metadata.
	// Use to set ingress class, NGINX configuration snippets, etc.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// KernelRequirements specifies which kernel services the app requires.
type KernelRequirements struct {
	// Identity specifies OIDC and/or LDAP requirements.
	// +optional
	Identity *IdentityRequirement `json:"identity,omitempty"`

	// Database specifies a relational database requirement.
	// +optional
	Database *DatabaseRequirement `json:"database,omitempty"`

	// Storage specifies object storage (S3) and/or filesystem (WebDAV) requirements.
	// +optional
	Storage *StorageRequirement `json:"storage,omitempty"`

	// Cache specifies a caching backend requirement.
	// +optional
	Cache *CacheRequirement `json:"cache,omitempty"`

	// Mail specifies SMTP and/or IMAP requirements.
	// +optional
	Mail *MailRequirement `json:"mail,omitempty"`

	// MCP specifies a Model Context Protocol server requirement.
	// When enabled, the orchestrator registers the app's MCP endpoint in the
	// kernel MCP registry and wires OIDC authentication.
	// +optional
	MCP *MCPRequirement `json:"mcp,omitempty"`
}

// IdentityRequirement specifies OIDC and/or LDAP needs.
type IdentityRequirement struct {
	// OIDC requests an OIDC client registration in the tenant's Keycloak realm.
	// +optional
	OIDC bool `json:"oidc,omitempty"`

	// LDAP requests a per-tenant LDAP bind account in the UCS LDAP directory.
	// +optional
	LDAP *LDAPRequirement `json:"ldap,omitempty"`
}

// LDAPRequirement describes per-tenant LDAP needs.
type LDAPRequirement struct {
	// Sync enables periodic user/group sync from LDAP into the app's own store.
	// +optional
	Sync bool `json:"sync,omitempty"`

	// Interval is the sync interval (e.g., "1h"). Defaults to "1h" when sync is true.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h|d)$`
	Interval string `json:"interval,omitempty"`
}

// DatabaseRequirement specifies a relational database need.
type DatabaseRequirement struct {
	// Engine selects the database engine. Defaults to postgresql.
	// +optional
	// +kubebuilder:default=postgresql
	Engine DatabaseEngine `json:"engine,omitempty"`

	// DatabasePerTenant creates a separate database for each tenant (default true).
	// Set to false only for shared-schema apps.
	// +optional
	// +kubebuilder:default=true
	DatabasePerTenant bool `json:"databasePerTenant"`
}

// StorageRequirement specifies object storage and/or filesystem needs.
type StorageRequirement struct {
	// S3 requests a per-tenant S3 bucket via the MinIO kernel service.
	// +optional
	S3 *S3Requirement `json:"s3,omitempty"`

	// Files requests WebDAV access to the tenant's Nextcloud instance.
	// +optional
	Files *FilesRequirement `json:"files,omitempty"`
}

// S3Requirement describes S3 storage needs.
type S3Requirement struct {
	// BucketPerTenant creates a dedicated bucket for each tenant. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	BucketPerTenant bool `json:"bucketPerTenant"`
}

// FilesRequirement describes WebDAV filesystem access needs.
type FilesRequirement struct {
	// Protocol is the access protocol. Currently only webdav is supported.
	// +kubebuilder:validation:Enum=webdav
	// +kubebuilder:default=webdav
	Protocol string `json:"protocol,omitempty"`

	// Capabilities lists the required access capabilities.
	// +optional
	// +kubebuilder:validation:Items:Enum=read;write
	Capabilities []string `json:"capabilities,omitempty"`
}

// CacheRequirement specifies a caching backend need.
type CacheRequirement struct {
	// Engine selects the caching backend. Defaults to redis.
	// +optional
	// +kubebuilder:default=redis
	Engine CacheEngine `json:"engine,omitempty"`
}

// MailRequirement specifies SMTP and/or IMAP needs.
type MailRequirement struct {
	// SMTP requests outbound mail (SMTP submission) credentials.
	// +optional
	SMTP *SMTPRequirement `json:"smtp,omitempty"`

	// IMAP requests inbound mail (IMAP) credentials.
	// +optional
	IMAP *IMAPRequirement `json:"imap,omitempty"`
}

// SMTPRequirement describes SMTP submission needs.
type SMTPRequirement struct {
	// Auth is the SMTP authentication mechanism.
	// +optional
	// +kubebuilder:validation:Enum=plain;login;cram-md5
	Auth string `json:"auth,omitempty"`

	// Port is the SMTP submission port.
	// +optional
	// +kubebuilder:default=587
	Port int32 `json:"port,omitempty"`
}

// IMAPRequirement describes IMAP access needs.
type IMAPRequirement struct {
	// Port is the IMAP port.
	// +optional
	// +kubebuilder:default=993
	Port int32 `json:"port,omitempty"`
}

// MCPRequirement describes a Model Context Protocol server endpoint.
type MCPRequirement struct {
	// Enabled activates MCP endpoint registration.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// Endpoint is the HTTP path where the app exposes its MCP server.
	// +optional
	// +kubebuilder:default=/mcp
	// +kubebuilder:validation:Pattern=`^/.*`
	Endpoint string `json:"endpoint,omitempty"`

	// Auth is the authentication method for MCP calls.
	// +optional
	// +kubebuilder:validation:Enum=oidc;none
	// +kubebuilder:default=oidc
	Auth string `json:"auth,omitempty"`
}

// ChartRef references an upstream Helm chart.
type ChartRef struct {
	// Repository is the OCI repository URL (e.g., oci://registry.example.com/charts).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Name is the chart name within the repository.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the chart version to deploy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
}

// ValueMapping declares which Helm value keys receive kernel-provided values.
// The orchestrator validates this schema at admission time and uses it to
// render ExternalSecret targets and Helm values.
type ValueMapping struct {
	// OIDC maps OIDC provider values to Helm keys.
	// +optional
	OIDC *OIDCValueMapping `json:"oidc,omitempty"`

	// Database maps database connection values to Helm keys.
	// +optional
	Database *DatabaseValueMapping `json:"database,omitempty"`

	// S3 maps object storage values to Helm keys.
	// +optional
	S3 *S3ValueMapping `json:"s3,omitempty"`

	// Cache maps caching backend values to Helm keys.
	// +optional
	Cache *CacheValueMapping `json:"cache,omitempty"`

	// SMTP maps mail submission values to Helm keys.
	// +optional
	SMTP *SMTPValueMapping `json:"smtp,omitempty"`

	// IMAP maps mail access values to Helm keys.
	// +optional
	IMAP *IMAPValueMapping `json:"imap,omitempty"`

	// LDAP maps LDAP directory values to Helm keys.
	// +optional
	LDAP *LDAPValueMapping `json:"ldap,omitempty"`
}

// OIDCValueMapping maps OIDC provider values to Helm chart keys.
// All fields are dot-notation Helm value paths (e.g., "oidc.clientId").
// Deeply nested paths with special characters in keys must use bracket notation
// (e.g., `appsuite.core-mw.secretProperties["com.openexchange.oidc.clientSecret"]`).
type OIDCValueMapping struct {
	// IssuerKey is the Helm value key for the OIDC issuer URL.
	// +optional
	IssuerKey string `json:"issuerKey,omitempty"`
	// ClientIDKey is the Helm value key for the OIDC client ID.
	// +optional
	ClientIDKey string `json:"clientIdKey,omitempty"`
	// ClientSecretKey is the Helm value key for the OIDC client secret.
	// +optional
	ClientSecretKey string `json:"clientSecretKey,omitempty"`
}

// DatabaseValueMapping maps database connection values to Helm chart keys.
type DatabaseValueMapping struct {
	// HostKey is the Helm value key for the database host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the database port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
	// NameKey is the Helm value key for the database name.
	// +optional
	NameKey string `json:"nameKey,omitempty"`
	// UserKey is the Helm value key for the database user.
	// +optional
	UserKey string `json:"userKey,omitempty"`
	// PasswordKey is the Helm value key for the database password.
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// S3ValueMapping maps S3 object storage values to Helm chart keys.
type S3ValueMapping struct {
	// EndpointKey is the Helm value key for the S3 endpoint URL.
	// +optional
	EndpointKey string `json:"endpointKey,omitempty"`
	// BucketKey is the Helm value key for the bucket name.
	// +optional
	BucketKey string `json:"bucketKey,omitempty"`
	// AccessKeyKey is the Helm value key for the S3 access key ID.
	// +optional
	AccessKeyKey string `json:"accessKeyKey,omitempty"`
	// SecretKeyKey is the Helm value key for the S3 secret access key.
	// +optional
	SecretKeyKey string `json:"secretKeyKey,omitempty"`
	// RegionKey is the Helm value key for the S3 region.
	// +optional
	RegionKey string `json:"regionKey,omitempty"`
}

// CacheValueMapping maps caching backend connection values to Helm chart keys.
type CacheValueMapping struct {
	// HostKey is the Helm value key for the cache host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the cache port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
	// PasswordKey is the Helm value key for the cache password/ACL token.
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// SMTPValueMapping maps SMTP submission values to Helm chart keys.
type SMTPValueMapping struct {
	// HostKey is the Helm value key for the SMTP host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the SMTP port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
	// UserKey is the Helm value key for the SMTP username.
	// +optional
	UserKey string `json:"userKey,omitempty"`
	// PasswordKey is the Helm value key for the SMTP password.
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// IMAPValueMapping maps IMAP access values to Helm chart keys.
type IMAPValueMapping struct {
	// HostKey is the Helm value key for the IMAP host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the IMAP port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
}

// LDAPValueMapping maps LDAP directory values to Helm chart keys.
// Supports deeply nested key paths for apps like OX App Suite that store
// LDAP config in properties files.
type LDAPValueMapping struct {
	// HostKey is the Helm value key for the LDAP host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the LDAP port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
	// BaseDNKey is the Helm value key for the LDAP base DN.
	// +optional
	BaseDNKey string `json:"baseDnKey,omitempty"`
	// BindDNKey is the Helm value key for the LDAP bind DN.
	// +optional
	BindDNKey string `json:"bindDnKey,omitempty"`
	// BindPasswordKey is the Helm value key for the LDAP bind password.
	// Supports bracket notation for deeply nested propertiesFiles paths, e.g.:
	// `appsuite.core-mw.propertiesFiles["/opt/.../ldapauth.properties"].bindDNPassword`
	// +optional
	BindPasswordKey string `json:"bindPasswordKey,omitempty"`
}

// AppSecret declares an app-internal secret the orchestrator must generate
// and inject. These are credentials the app needs (admin passwords, session
// signing keys, cluster tokens) that are not provided by any kernel service.
type AppSecret struct {
	// Name is the logical identifier for this secret (e.g., "admin_password").
	// Used as the suffix in the OpenBao path:
	// gentian-os/tenants/{tenant}/apps/{app}/internal/{name}
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9_]+$`
	Name string `json:"name"`

	// ValuePath is the dot-notation (or bracket-notation) Helm value key that
	// should receive the generated secret value.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ValuePath string `json:"valuePath"`
}

// ContractRef identifies an integration contract this app provides.
type ContractRef struct {
	// Name is the contract name (e.g., "file-store", "project-management").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Name string `json:"name"`

	// Protocol is the technical protocol used (e.g., "http-json", "webdav").
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// IntegrationRef identifies an optional integration contract this app can consume.
type IntegrationRef struct {
	// Contract is the name of the contract to consume.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Contract string `json:"contract"`

	// Provider is the expected provider app by profile name.
	// +optional
	Provider string `json:"provider,omitempty"`

	// Capabilities lists the specific capabilities required from the contract.
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`
}

// AppProfile is the Schema for the appprofiles API.
//
// AppProfile is cluster-scoped — one per app type, shared across all tenants.
// An AppProfile wraps an upstream Helm chart with the schema-based value
// mapping and kernel requirements that the orchestrator needs to provision
// and wire the app for any tenant.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ap;aps
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Chart",type=string,JSONPath=`.spec.chart.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.chart.version`
// +kubebuilder:printcolumn:name="Method",type=string,JSONPath=`.spec.deploymentMethod`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AppProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AppProfileSpec `json:"spec,omitempty"`
}

// AppProfileList contains a list of AppProfile.
// +kubebuilder:object:root=true
type AppProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppProfile{}, &AppProfileList{})
}
