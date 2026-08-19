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

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TenantSpec defines the desired state of a Tenant.
type TenantSpec struct {
	// DisplayName is a human-readable name for this tenant/organisation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	DisplayName string `json:"displayName"`

	// Domain is the optional custom domain for this tenant's app zone (e.g.
	// `acme.com`). When unset, the effective domain depends on cluster
	// tenancy mode: multi → `<tenant-name>.<KERNEL_DOMAIN>`; single →
	// `<KERNEL_DOMAIN>` (flat URLs). See docs/design/multi-tenancy.md §3.
	// In both cases the operator issues a per-tenant wildcard TLS certificate
	// for *.<effectiveDomain> via DNS-01. See docs/design/multi-tenancy.md §3.
	// +optional
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([a-z0-9\-\.]*[a-z0-9])?)?$`
	Domain string `json:"domain,omitempty"`

	// AdminEmail is the contact address for platform notifications.
	//
	// Derived when empty, as `admin@<effectiveDomain>` — so a tenant in multi
	// mode gets admin@<tenant>.<KERNEL_DOMAIN>, and one in single mode
	// admin@<KERNEL_DOMAIN>. Required, it had to be written by hand for every
	// tenant, and a definition copied between clusters carried the other
	// cluster's domain into an address nothing would ever deliver to.
	//
	// Set it only to override that: a contact address outside the tenant's own
	// domain, which is a decision rather than a default.
	// +optional
	// +kubebuilder:validation:Pattern=`^([^@\s]+@[^@\s]+\.[^@\s]+)?$`
	AdminEmail string `json:"adminEmail,omitempty"`

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

	// MaxPods caps the number of pods in the tenant namespace (init Jobs + app workloads).
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxPods int32 `json:"maxPods,omitempty"`
}

// TenantApp specifies a desired application installation for a tenant.
//
// +kubebuilder:validation:XValidation:rule="has(self.profile) || has(self.profileRef)",message="either profile or profileRef is required"
type TenantApp struct {
	// Profile is the name of the AppProfile CR to install.
	// When profileRef is set, the operator resolves it to a concrete profile name
	// and may populate this field for observability.
	// +optional
	Profile string `json:"profile,omitempty"`

	// ProfileRef selects an AppProfile by catalogue identity (family, version, edition,
	// offering tier). Takes precedence over profile when resolving installs.
	// +optional
	ProfileRef *ProfileReference `json:"profileRef,omitempty"`

	// Config provides per-tenant overrides for this app installation.
	// Values here are merged over the AppProfile's extraValues.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *TenantAppConfig `json:"config,omitempty"`

	// Addons lists the addon profiles activated inside this app for this tenant —
	// customization-ladder rung L3. Each entry names an AppProfile carrying
	// gentianos.io/deployment-role: addon in the same family as this app.
	//
	// Addons are activation state *inside* the installed app, not separate
	// installs: they never appear as their own entries in spec.apps, get no App
	// claim of their own, and are applied through the app's native mechanism
	// (Odoo `-i`, Nextcloud `occ app:enable`).
	//
	// The App Store writes this list — pre-filled from an AppPackage preset when
	// one is chosen, then editable afterwards. Entries the tenant is not entitled
	// to are rejected; entitlement is what gates an ee addon, not compatibility.
	// See gentian-os/docs/app-customization.md §4.2.
	// +optional
	// +listType=set
	Addons []string `json:"addons,omitempty"`
}

// TenantAppConfig holds per-tenant application overrides.
type TenantAppConfig struct {
	// Replicas overrides the default replica count.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// ExtraValues deep-merges Helm values over the AppProfile defaults.
	// Must not contain secrets — use valueMapping for credentials.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraValues *runtime.RawExtension `json:"extraValues,omitempty"`

	// DropIns supply tenant-authored files into drop-in directories the target
	// AppProfile declares as tenantEditable. This is rung L1 at tenant scope —
	// the highest rung a tenant admin may reach unaided, and the only one where
	// self-service makes sense: content, never code.
	//
	// Each entry must name a declared, tenantEditable drop-in; filenames are
	// restricted to the 90-99 range so platform and profile files keep priority.
	// Content lands in a ConfigMap and is therefore not secret material.
	// See docs/app-customization.md §2.2.1.
	// +optional
	// +listType=map
	// +listMapKey=name
	DropIns []TenantAppDropIn `json:"dropIns,omitempty"`
}

// TenantAppDropIn is tenant-authored content for one declared drop-in directory.
type TenantAppDropIn struct {
	// Name must match an AppProfile.spec.customization.dropIns[].name entry that
	// sets tenantEditable: true. Tenants cannot invent mount paths — that would be
	// repackaging (L4) at tenant scope.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Files maps filename to content. Filenames are validated against the
	// tenant-reserved 90-99 numeric prefix range, and total content is capped by
	// the drop-in's maxBytes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinProperties=1
	Files map[string]string `json:"files"`
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

	// AdminEmail is the resolved contact address for this tenant — spec.adminEmail
	// when set, otherwise admin@<effective domain>.
	//
	// Published because the resolved value is what an operator signs in with, and
	// spec.adminEmail is empty in the normal case where it is derived. A consumer
	// reading only the spec sees nothing and has to re-derive it, which is how
	// `gtnctl tenants deploy` came to print admin-<tenant>@gentian.org: a domain
	// belonging to no cluster, for an account that does not exist.
	// +optional
	AdminEmail string `json:"adminEmail,omitempty"`
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
// creation, Keycloak realm, per-app databases/buckets/cache, and
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

// AdminEmailOrDefault returns the tenant's contact address: spec.adminEmail
// when set, otherwise `admin@<effectiveDomain>`.
//
// One rule, here rather than at each call site: the address goes into the
// provisioning Job and into the XTenant, whose XRD requires it, and two copies
// of a derivation are two things to keep in step.
//
// The local part is `admin`, not the generated login. The login carries the
// tenant name so it stays unique across realms (admin-corp); inside the
// tenant's own domain that reads as admin-corp@corp.example, naming the tenant
// twice. The mailbox belongs to the domain, so the domain says whose it is.
// MailDomainOrDefault is the domain this tenant's mail is addressed in.
//
// spec.mail.domain when set, otherwise the tenant's ingress domain. The two are
// usually the same and are allowed to differ: a cluster can put every tenant's
// mail on one domain while each still serves its apps on its own subdomain.
//
// On the API type rather than in the mail controller because the admin address
// is derived from it too, and those two were deriving it separately — see
// AdminEmailOrDefault.
func (t *Tenant) MailDomainOrDefault(kernelDomain, tenancyMode string) string {
	if t.Spec.Mail != nil && t.Spec.Mail.Domain != "" {
		return t.Spec.Mail.Domain
	}
	return t.EffectiveDomain(kernelDomain, tenancyMode)
}

// AdminEmailOrDefault is the tenant administrator's address.
//
// Derived from the MAIL domain, not the ingress domain. Those are the same
// unless spec.mail.domain says otherwise, and when it does, an address in the
// ingress domain is one no mailbox exists for: Postfix accepts mail only for
// the domains in virtual_mailbox_domains, which the operator writes from the
// mail domain. That mismatch is why tenant definitions carried hand-written
// addresses — the derived one could not receive.
//
// The local part is `admin` when the tenant has the domain to itself, and
// `admin-<tenant>` when the mail domain is shared with other tenants. Sharing
// is what makes the tenant name necessary: two tenants on one mail domain would
// otherwise both be admin@ that domain, and the second to provision would
// collide with the first. Inside the tenant's own domain the same name would
// appear twice — admin-corp@corp.example — which is why it is not added there.
func (t *Tenant) AdminEmailOrDefault(kernelDomain, tenancyMode string) string {
	if t.Spec.AdminEmail != "" {
		return t.Spec.AdminEmail
	}
	domain := t.MailDomainOrDefault(kernelDomain, tenancyMode)
	if domain == "" {
		// No domain configured at all: .invalid is reserved by RFC 2606 and can
		// never resolve, which is the honest representation of "unknown".
		return "admin-" + t.Name + "@" + t.Name + ".invalid"
	}
	if domain == t.EffectiveDomain(kernelDomain, tenancyMode) {
		return "admin@" + domain
	}
	return "admin-" + t.Name + "@" + domain
}

// EffectiveDomain returns the domain to use for ingress and mail routing
// for this tenant. It returns spec.domain if set. When spec.domain is unset,
// multi-tenancy mode uses "<tenant-name>.<kernelDomain>"; single-tenancy mode
// uses "<kernelDomain>" (flat app hostnames). An empty kernelDomain combined
// with an empty spec.domain returns the empty string — callers must treat that
// as a configuration error and skip ingress provisioning.
//
// See docs/design/multi-tenancy.md §3.
func (t *Tenant) EffectiveDomain(kernelDomain, tenancyMode string) string {
	if t.Spec.Domain != "" {
		return t.Spec.Domain
	}
	if kernelDomain == "" {
		return ""
	}
	if NormalizeTenancyMode(tenancyMode) == TenancyModeSingle {
		return kernelDomain
	}
	return t.Name + "." + kernelDomain
}

// HasVanityDomain reports whether the tenant has an explicit custom app domain
// configured (i.e. spec.domain is set). When false, EffectiveDomain() derives
// the zone from tenancy mode and tenant name. TLS uses the same per-tenant
// wildcard model in both cases; only the effective domain string differs.
func (t *Tenant) HasVanityDomain() bool {
	return t.Spec.Domain != ""
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
