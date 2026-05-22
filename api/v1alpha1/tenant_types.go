package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantSpec defines the desired state of a Tenant.
type TenantSpec struct {
	// DisplayName is a human-readable name for this tenant/organisation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	DisplayName string `json:"displayName"`

	// Domain is the optional vanity domain for this tenant's apps (e.g.
	// `acme.com`). When unset, the operator falls back to
	// `<tenant-name>.<KERNEL_DOMAIN>` (e.g. `gtn-demo.desk.gentian.org`),
	// served under the kernel wildcard certificate. See
	// docs/architecture.md §2.5.
	// +optional
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([a-z0-9\-\.]*[a-z0-9])?)?$`
	Domain string `json:"domain,omitempty"`

	// AdminEmail is the contact address for platform notifications.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[^@\s]+@[^@\s]+\.[^@\s]+$`
	AdminEmail string `json:"adminEmail"`

	// Isolation describes the workload isolation boundaries for this tenant.
	// +optional
	Isolation *TenantIsolation `json:"isolation,omitempty"`

	// Mail configures the mail mode and settings for this tenant.
	// +optional
	Mail *TenantMail `json:"mail,omitempty"`

	// Quotas sets resource limits for this tenant.
	// +optional
	Quotas *TenantQuotas `json:"quotas,omitempty"`

	// DeletionPolicy controls behaviour when the Tenant CR is deleted.
	// Defaults to Retain.
	// +optional
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// Apps lists the applications to install for this tenant.
	// +optional
	Apps []TenantApp `json:"apps,omitempty"`

	// Office configures the Nextcloud Office (Collabora) document editing extension
	// for this tenant. When enabled, the shared kernel Collabora service provides
	// WOPI-based document editing inside Nextcloud.
	// +optional
	Office *TenantOffice `json:"office,omitempty"`
}

// TenantOffice configures the Nextcloud Office (Collabora) document editing extension.
// Collabora is a shared kernel service — one instance serves all tenants.
type TenantOffice struct {
	// Enabled activates the Collabora WOPI document editor for this tenant.
	// Defaults to false.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`
}

// TenantIsolation describes the namespace and identity boundaries.
type TenantIsolation struct {
	// Mode selects the isolation strategy. Defaults to namespace.
	// +optional
	// +kubebuilder:default=namespace
	Mode IsolationMode `json:"mode,omitempty"`

	// Namespace overrides the target namespace name.
	// Defaults to "tenant-{tenant-name}" when not set.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9\-]*[a-z0-9]$`
	Namespace string `json:"namespace,omitempty"`

	// LDAPOu is the LDAP OU path for this tenant's users and groups.
	// Defaults to "ou={tenant-name}" under the root DN.
	// +optional
	LDAPOu string `json:"ldapOU,omitempty"`

	// KeycloakRealm is the Keycloak realm name for this tenant.
	// Defaults to the tenant name.
	// +optional
	KeycloakRealm string `json:"keycloakRealm,omitempty"`

	// DatabasePrefix is the prefix for all database names belonging to this tenant.
	// Defaults to "{tenant-name}_".
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9_]*$`
	DatabasePrefix string `json:"databasePrefix,omitempty"`

	// S3Prefix is the prefix for all S3 bucket names belonging to this tenant.
	// Defaults to "{tenant-name}-".
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9\-]*$`
	S3Prefix string `json:"s3Prefix,omitempty"`
}

// TenantMail configures the mail stack for this tenant.
type TenantMail struct {
	// Mode selects the mail delivery strategy. Defaults to selfhosted.
	// +optional
	// +kubebuilder:default=selfhosted
	Mode MailMode `json:"mode,omitempty"`

	// Domain is the mail domain. Defaults to spec.domain.
	// +optional
	Domain string `json:"domain,omitempty"`

	// QuotaPerUser is the per-user mailbox storage quota.
	// +optional
	QuotaPerUser *resource.Quantity `json:"quotaPerUser,omitempty"`

	// RateLimit is the outbound email rate limit (e.g., "100/h").
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+/(s|m|h|d)$`
	RateLimit string `json:"rateLimit,omitempty"`

	// SmtpCredentialsSecret is the name of an existing Kubernetes Secret in the
	// kernel namespace that contains SMTP relay credentials for external mail
	// delivery. Required when mode=external.
	// The Secret must provide keys: host, port, username, password.
	// +optional
	SmtpCredentialsSecret string `json:"smtpCredentialsSecret,omitempty"`
}

// TenantQuotas defines resource consumption limits for this tenant.
type TenantQuotas struct {
	// MaxApps is the maximum number of apps this tenant may install.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxApps int32 `json:"maxApps,omitempty"`

	// Storage is the total storage quota (PVCs + S3 buckets).
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`

	// CPU is the total CPU request limit across all tenant pods.
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`

	// Memory is the total memory request limit across all tenant pods.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// TenantApp specifies a desired application installation for a tenant.
type TenantApp struct {
	// Profile is the name of the AppProfile CR to install.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Profile string `json:"profile"`

	// IsolationMode overrides the AppProfile's default deployment mode for this
	// tenant. "dedicated" provisions a separate per-tenant deployment in the
	// tenant namespace. "shared" provisions a single shared deployment in the
	// platform-kernel namespace; all tenants with the same profile and "shared"
	// mode share one Helm release and OIDC client, with per-tenant IAM brokering
	// via the shared-apps Keycloak realm.
	// Defaults to the AppProfile's isolation.default (which itself defaults to "dedicated").
	// +optional
	IsolationMode AppDeploymentMode `json:"isolationMode,omitempty"`

	// Config provides per-tenant overrides for this app installation.
	// Values here are merged over the AppProfile's extraValues.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *TenantAppConfig `json:"config,omitempty"`
}

// TenantAppConfig holds per-tenant application overrides.
type TenantAppConfig struct {
	// Replicas overrides the default replica count.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
}

// TenantStatus holds the observed state of a Tenant.
type TenantStatus struct {
	// Phase summarises the overall lifecycle state.
	// +optional
	Phase TenantPhase `json:"phase,omitempty"`

	// Conditions provides detailed status conditions using the standard
	// metav1.Condition type.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ProvisionedApps lists apps that have been successfully provisioned.
	// +optional
	ProvisionedApps []string `json:"provisionedApps,omitempty"`

	// AppCount is the total number of apps requested in spec.
	// +optional
	AppCount int `json:"appCount,omitempty"`

	// ReadyApps is the number of apps that have been successfully provisioned.
	// +optional
	ReadyApps int `json:"readyApps,omitempty"`
	// Namespace is the resolved tenant namespace name.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// ObservedGeneration is the last processed generation of the spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Mail holds the observed DNS record data produced by the mail reconciler.
	// Operators must publish these values in the tenant's DNS zone.
	// +optional
	Mail *TenantMailStatus `json:"mail,omitempty"`
}

// TenantMailStatus holds DNS record data emitted by the mail reconciler for
// the selfhosted mail mode. Operators must publish these in the tenant's DNS zone.
type TenantMailStatus struct {
	// DKIMPublicKey is the RSA public key for DKIM signing, base64-encoded (PKIX DER).
	// Publish as a TXT record: v=DKIM1; k=rsa; p=<DKIMPublicKey>
	// under mail._domainkey.<mail domain>.
	// +optional
	DKIMPublicKey string `json:"dkimPublicKey,omitempty"`

	// SPFRecord is the suggested SPF TXT record value for the mail domain.
	// +optional
	SPFRecord string `json:"spfRecord,omitempty"`

	// DMARCRecord is the suggested DMARC TXT record value for _dmarc.<mail domain>.
	// +optional
	DMARCRecord string `json:"dmarcRecord,omitempty"`
}

// Tenant is the Schema for the tenants API.
//
// Tenant is cluster-scoped and represents a customer organisation. Creating a
// Tenant CR triggers the orchestrator's full provisioning pipeline: namespace
// creation, LDAP OU, Keycloak realm, per-app databases/buckets/cache, and
// ArgoCD Application (or Crossplane App claim) CRs for each requested app.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tenant;tenants
// +kubebuilder:printcolumn:name="STATUS",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="APPS",type=integer,JSONPath=`.status.appCount`
// +kubebuilder:printcolumn:name="READY",type=integer,JSONPath=`.status.readyApps`
// +kubebuilder:printcolumn:name="MAIL",type=string,JSONPath=`.spec.adminEmail`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// TenantList contains a list of Tenant.
// +kubebuilder:object:root=true
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

// EffectiveDomain returns the domain to use for ingress and mail routing
// for this tenant. It returns spec.domain if set, otherwise it falls back
// to "<tenant-name>.<kernelDomain>". An empty kernelDomain combined with
// an empty spec.domain returns the empty string — callers must treat that
// as a configuration error and skip ingress provisioning.
//
// See docs/architecture.md §2.5 (Domains and TLS).
func (t *Tenant) EffectiveDomain(kernelDomain string) string {
	if t.Spec.Domain != "" {
		return t.Spec.Domain
	}
	if kernelDomain == "" {
		return ""
	}
	return t.Name + "." + kernelDomain
}

// HasVanityDomain reports whether the tenant has an explicit vanity domain
// configured (i.e. spec.domain is set). When false, the tenant uses the
// `<tenant>.<kernel_domain>` fallback covered by the kernel wildcard cert.
func (t *Tenant) HasVanityDomain() bool {
	return t.Spec.Domain != ""
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
