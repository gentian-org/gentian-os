/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TenantSpec defines the desired state of a Tenant.
// TenantAdmin is the handover of a tenant's administrator account.
type TenantAdmin struct {
	// RequireMFA makes enrolling a second factor part of activating the
	// account. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	RequireMFA *bool `json:"requireMFA,omitempty"`
}

// TenantDeletion is what a purge (deletionPolicy: Delete) leaves behind.
type TenantDeletion struct {
	// KeepBundles keeps the tenant's backup bucket and the bundles in it
	// when everything else is deleted: the offboarding case, where the
	// tenant was handed a copy and a provider keeps one under contract.
	// Off by default -- a purge that leaves the backups behind has not
	// purged.
	// +optional
	KeepBundles bool `json:"keepBundles,omitempty"`
}

// KeepsBundles reports whether a Delete spares the backup bucket.
func (t *Tenant) KeepsBundles() bool {
	return t.Spec.Deletion != nil && t.Spec.Deletion.KeepBundles
}

// AdminRequiresMFA reports whether the administrator must enrol a second
// factor; true unless the tenant says otherwise.
func (t *Tenant) AdminRequiresMFA() bool {
	if t.Spec.Admin == nil || t.Spec.Admin.RequireMFA == nil {
		return true
	}
	return *t.Spec.Admin.RequireMFA
}

type TenantSpec struct {
	// DisplayName is a human-readable name for this tenant/organisation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	DisplayName string `json:"displayName"`

	// Admin is how the tenant's administrator account is handed over. The
	// account is created with no password: its holder sets one through a
	// single-use, expiring link, mailed to a recovery address when the person
	// activating it gives one and otherwise shown, once, to them. The address
	// is not here: a person's address committed to git outlives the account.
	// +optional
	Admin *TenantAdmin `json:"admin,omitempty"`

	// Isolation describes the workload isolation boundaries for this tenant.
	// +optional
	Isolation *TenantIsolation `json:"isolation,omitempty"`

	// Mail configures the mail mode and settings for this tenant.
	// +optional
	Mail *TenantMail `json:"mail,omitempty"`

	// Quotas sets resource limits for this tenant.
	// +optional
	Quotas *TenantQuotas `json:"quotas,omitempty"`

	// Security is the realm policy this tenant runs under: how strong a
	// password has to be, how long a session lasts, what happens after
	// repeated failures. Unset leaves Keycloak's own defaults.
	//
	// Declared here rather than set through Keycloak's admin API, which is
	// what the console used to do. The composition turns this into the
	// realm's fields, so the policy is reviewable in git, survives a realm
	// being rebuilt, and needs no credential anywhere: the thing that changes
	// it is a commit, and the thing that applies it is the reconciler that
	// owns the realm.
	// +optional
	Security *TenantSecurity `json:"security,omitempty"`

	// DeletionPolicy controls behaviour when the Tenant CR is deleted.
	// Defaults to Retain.
	// +optional
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// Deletion refines what a Delete leaves behind.
	// +optional
	Deletion *TenantDeletion `json:"deletion,omitempty"`

	// Apps lists the applications to install for this tenant.
	// +optional
	Apps []TenantApp `json:"apps,omitempty"`

	// Catalogue is where this tenant may install from beside the catalogues
	// the whole cluster offers: the catalogues only this tenant sees, and
	// whether its own administrators may add them.
	//
	// The director reads and writes it, in the tenant's manifest in git.
	// Nothing in the cluster acts on it: what an install may use is decided
	// when the director resolves a coordinate, and what a tenant may roll out
	// is decided by the origin a materialised profile carries.
	// +optional
	Catalogue *TenantCatalogue `json:"catalogue,omitempty"`

	// Locales are the languages this tenant's realm renders its login and
	// account pages in, as ISO 639-1 codes (AD-15). Empty means the
	// platform's own set.
	//
	// Languages rather than locales: Keycloak serves de-CH from its German
	// catalogue, and a realm listing regional codes offers a picker full of
	// entries that render identically.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Pattern=`^[a-z]{2}(-[A-Za-z0-9]{2,8})?$`
	Locales []string `json:"locales,omitempty"`

	// Privileges are the privilege requests a person granted for this
	// tenant's components (AD-5). They live here because a grant has to
	// survive the thing it applies to: a Component is rebuilt from its
	// profile and can be deleted and recreated, while the record that a named
	// person said yes on a named date must not be. The Tenant comes from git,
	// so this is also what makes an approval a commit rather than an edit
	// somebody made to a live object.
	//
	// The operator copies each entry onto the Component named by Install. A
	// grant for a component that does not exist is kept and ignored, because
	// an app can be uninstalled and reinstalled and re-asking for an approval
	// that was already given is how approvals become a formality.
	// +optional
	// +listType=map
	// +listMapKey=install
	// +listMapKey=privilege
	// +kubebuilder:validation:MaxItems=256
	Privileges []TenantPrivilegeGrant `json:"privileges,omitempty"`

	// Exposures are the perimeter surfaces a perimeter approver published
	// (AD-6): what this tenant has on the internet, from when, until when,
	// and who said so.
	//
	// Here for the same reason the grants are: the decision has to outlive
	// the thing it applies to, and it has to be a commit rather than an edit
	// somebody made to a live object. The operator copies each entry onto the
	// Component it names, and the Component's reconcile is what stands the
	// proxy up in the tenant's DMZ.
	//
	// It is also the registry. Every URL this tenant publishes is one of
	// these, which is what lets "what of ours is on the internet" be answered
	// by the thing that put it there.
	// +optional
	// +listType=map
	// +listMapKey=install
	// +listMapKey=exposureName
	// +kubebuilder:validation:MaxItems=256
	Exposures []TenantExposure `json:"exposures,omitempty"`
}

// TenantExposure is one published surface, against one of the tenant's
// components.
//
// The fields are spelled out rather than embedded so the two list map keys can
// be the component and the entry: together they are what makes a published
// surface unique, and letting the API server enforce that is better than a
// controller finding two answers to how long something is on the internet.
type TenantExposure struct {
	// Install names the Component this publishes from.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=253
	Install string `json:"install"`

	// ExposureName names an entry of that component's profile whose surface
	// is perimeter. An entry that is not is never published here: the
	// operator checks, because enabling a gateway entry by name would
	// otherwise put the component's authenticated surface on the internet.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	ExposureName string `json:"exposureName"`

	// Owner is the subject that published it, set by the director from the
	// caller's token.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Owner string `json:"owner"`

	// ExpiresAt is when it stops answering, for a surface published for a
	// while. Optional: one meant to stay has none.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// ReviewAt is when the owner and the approver look at it again. Always
	// set; overdue is reported and takes nothing down.
	ReviewAt metav1.Time `json:"reviewAt"`

	// Reason is why this is public, in the approver's words.
	// +optional
	// +kubebuilder:validation:MaxLength=2000
	Reason string `json:"reason,omitempty"`

	// PublishedAt is when it was first published. It does not change when
	// the entry is reviewed.
	// +optional
	PublishedAt *metav1.Time `json:"publishedAt,omitempty"`

	// LastReviewedBy is the subject that last confirmed this should stay
	// public, and LastReviewedAt when. Publishing is the first review, and
	// the owner may well be the reviewer every time: what is asked for is a
	// regular look, not a second person.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	LastReviewedBy string `json:"lastReviewedBy,omitempty"`
	// +optional
	LastReviewedAt *metav1.Time `json:"lastReviewedAt,omitempty"`
}

// Enablement is this entry as the Component carries it.
func (e *TenantExposure) Enablement() ExposureEnablement {
	return ExposureEnablement{
		ExposureName: e.ExposureName,
		Owner:        e.Owner,
		ExpiresAt:    e.ExpiresAt,
		ReviewAt:     e.ReviewAt,
	}
}

// TenantPrivilegeGrant is one grant, against one of the tenant's components.
//
// The fields of PrivilegeGrant are spelled out rather than embedded so that
// this is one flat object in the CRD and the two list map keys can be the
// component and the privilege -- together they are what makes a grant unique,
// and letting the API server enforce that is better than a controller
// discovering two answers to the same question.
type TenantPrivilegeGrant struct {
	// Install names the Component this grant is for, which is the component's
	// object name in the tenant's namespace.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=253
	Install string `json:"install"`

	// Privilege names one entry of the profile's request, as <kind>/<name>.
	// +kubebuilder:validation:Pattern=`^(podSecurity|egress|clusterRoles)/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=80
	Privilege string `json:"privilege"`

	// Approver is the Keycloak subject who said yes, set by the director from
	// the caller's token. Never supplied by a caller: a grant that could name
	// its own approver would record nothing.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Approver string `json:"approver"`

	ApprovedAt metav1.Time `json:"approvedAt"`

	// Reason in the approver's words, not the profile's. The profile already
	// said why it wants the privilege; this is why somebody agreed.
	// +kubebuilder:validation:MinLength=10
	// +kubebuilder:validation:MaxLength=2000
	Reason string `json:"reason"`

	// ExpiresAt bounds the grant. A waiver with no expiry is a waiver nobody
	// reviews.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// Grant is this entry as the Component carries it.
func (g *TenantPrivilegeGrant) Grant() PrivilegeGrant {
	return PrivilegeGrant{
		Privilege:  g.Privilege,
		Approver:   g.Approver,
		ApprovedAt: g.ApprovedAt,
		Reason:     g.Reason,
		ExpiresAt:  g.ExpiresAt,
	}
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

	// CPU caps the sum of container CPU **limits** in the namespace.
	//
	// A limit is a burst ceiling, not a reservation: it is what a container may
	// spike to, and the scheduler does not set anything aside for it. Chart
	// defaults are generous with limits — one tenant running Nextcloud, an
	// office suite and the App Store sums to roughly six cores of limits while
	// reserving one — so this number does not correspond to hardware and must
	// not be the one a plan is sold on. It exists to bound the blast radius of
	// a runaway container. Sell RequestsCPU.
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`

	// Memory caps the sum of container memory **limits** in the namespace.
	// The burst ceiling, for the same reason as CPU. Sell RequestsMemory.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`

	// RequestsCPU caps the sum of container CPU **requests** in the namespace.
	//
	// Requests are what the scheduler actually reserves, so this is the number
	// that maps one-to-one onto purchased capacity: two cores of requests is
	// two cores of a node that nothing else can schedule into. It is therefore
	// the quantity a ResourcePlan is priced on.
	//
	// Safe to impose on a namespace that already has pods: the tenant
	// LimitRange sets defaultRequest (100m / 128Mi), so a container that
	// declares no request still has one and the quota cannot reject it.
	// +optional
	RequestsCPU *resource.Quantity `json:"requestsCpu,omitempty"`

	// RequestsMemory caps the sum of container memory **requests** in the
	// namespace — reserved capacity, priced, as RequestsCPU is.
	// +optional
	RequestsMemory *resource.Quantity `json:"requestsMemory,omitempty"`

	// MaxPods caps the number of pods in the tenant namespace (init Jobs + app workloads).
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxPods int32 `json:"maxPods,omitempty"`
}

// TenantCatalogue is a tenant's own catalogues and who may add them.
type TenantCatalogue struct {
	// Delegated says the tenant's own administrators may add and remove
	// catalogues for this tenant. Off unless the cluster's administrator
	// turns it on; a tenant's administrator cannot.
	// +optional
	Delegated bool `json:"delegated,omitempty"`

	// Sources are the catalogues only this tenant sees.
	// +optional
	// +listType=map
	// +listMapKey=name
	Sources []TenantCatalogueSource `json:"sources,omitempty"`
}

// TenantCatalogueSource is one catalogue of a tenant: an address that serves
// index.yaml and profiles/<name>.yaml.
type TenantCatalogueSource struct {
	// Name is the catalogue's name: the first half of a coordinate,
	// <name>/<app>. It is not the name of a catalogue of the whole cluster,
	// nor of another catalogue of this tenant.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// URL is the https address the catalogue is served from.
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// AddedBy says who added it: the cluster's administrator, or the
	// tenant's own under delegation. A tenant's administrator removes only
	// what the tenant added.
	// +kubebuilder:validation:Enum=cluster;tenant
	// +kubebuilder:default=cluster
	// +optional
	AddedBy string `json:"addedBy,omitempty"`
}

// Who added a tenant's catalogue.
const (
	CatalogueAddedByCluster = "cluster"
	CatalogueAddedByTenant  = "tenant"
)

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

	// Digest pins the profile bundle this app was installed from: the build
	// the install asked for. It is a field of the install and not part of the
	// profile's name, so the same app keeps one name across builds.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest,omitempty"`

	// Catalogue is the catalogue the pinned build was fetched from: the first
	// half of its coordinate, <catalogue>/<profile>. It is written with the
	// digest by an install that fetched the bundle from that catalogue's
	// source, and is absent on every other entry.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Catalogue string `json:"catalogue,omitempty"`

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
	// one is chosen, then editable afterwards. Every entry that resolves is
	// activated, whatever its edition: the platform gates none, and a paid
	// addon arrives only where the tenant holds a credential for the
	// repository it is pulled from. See gentian-os/docs/app-customization.md §4.2.
	// +optional
	// +listType=set
	Addons []string `json:"addons,omitempty"`

	// AddonPins pins addons of this app to the build each was installed
	// from, like digest and catalogue do for the app itself. It is keyed by
	// the addon's name and stands beside addons rather than inside it: addons
	// stays the list of names every reader walks, and an addon with no entry
	// here is activated unpinned, as before.
	//
	// A pinned addon is rolled out only from a profile shown to be that
	// build. Until it is, the base it activates inside is held as it runs.
	// An entry naming an addon that is not in addons pins nothing.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=128
	AddonPins []AddonPin `json:"addonPins,omitempty"`

	// DefaultGrant says every member of the tenant has access to this app by
	// default. The people who are members when the app's group first exists
	// are added to it, once, and the group is marked so that somebody invited
	// later has the app pre-selected. It is applied once per install
	// (status.defaultGrantedApps): a person an administrator removes from the
	// group afterwards stays removed.
	//
	// Absent or false, access is given per person.
	// +optional
	DefaultGrant bool `json:"defaultGrant,omitempty"`
}

// AddonPin is the build one addon was installed from.
type AddonPin struct {
	// Name is the addon's profile name, as addons lists it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Digest pins the addon's profile bundle: the build the install asked
	// for.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// Catalogue is the catalogue the pinned build was fetched from: the first
	// half of its coordinate, <catalogue>/<name>.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Catalogue string `json:"catalogue,omitempty"`
}

// PinOf answers the digest an addon of this app is pinned to, or "" for an
// addon that is not pinned.
func (a TenantApp) PinOf(addon string) string {
	for _, pin := range a.AddonPins {
		if pin.Name == addon {
			return pin.Digest
		}
	}
	return ""
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

// TenantSecurity is the realm policy a tenant administrator may set.
//
// Every field maps to something the Keycloak realm already has. Nothing here
// is a Gentian invention on top: the console shows the realm's own controls,
// and the composition is what carries them across.
type TenantSecurity struct {
	// Password is how strong a password must be.
	// +optional
	Password *PasswordPolicy `json:"password,omitempty"`

	// Session is how long one lasts.
	// +optional
	Session *SessionPolicy `json:"session,omitempty"`

	// BruteForce is what happens after repeated failures.
	// +optional
	BruteForce *BruteForcePolicy `json:"bruteForce,omitempty"`
}

// PasswordPolicy becomes Keycloak's own password policy string.
//
// Split into fields rather than carried as that string, because a screen has
// to offer the parts and a reviewer has to read the diff. The composition
// assembles it; "length(12) and digits(1)" is not something anybody should
// have to write by hand into a tenant manifest.
type PasswordPolicy struct {
	// MinLength is the shortest password accepted. Zero leaves it unstated.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=128
	MinLength int32 `json:"minLength,omitempty"`

	// RequireDigits, RequireLowercase, RequireUppercase and
	// RequireSpecialChars each demand at least one of that kind.
	// +optional
	RequireDigits bool `json:"requireDigits,omitempty"`
	// +optional
	RequireLowercase bool `json:"requireLowercase,omitempty"`
	// +optional
	RequireUppercase bool `json:"requireUppercase,omitempty"`
	// +optional
	RequireSpecialChars bool `json:"requireSpecialChars,omitempty"`

	// HistoryCount refuses a password the person has used in their last N.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=64
	HistoryCount int32 `json:"historyCount,omitempty"`

	// MaxAgeDays forces a change after that many days. Zero means never,
	// which is what current guidance actually recommends: rotation on a
	// timer makes people choose worse passwords, and it is here because
	// some compliance regimes still demand it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3650
	MaxAgeDays int32 `json:"maxAgeDays,omitempty"`
}

// SessionPolicy is how long a sign-in lasts.
type SessionPolicy struct {
	// IdleMinutes ends a session that has done nothing for this long.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=43200
	IdleMinutes int32 `json:"idleMinutes,omitempty"`

	// MaxHours ends it regardless of activity.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8760
	MaxHours int32 `json:"maxHours,omitempty"`

	// RememberMe offers the longer session the realm is configured for.
	// +optional
	RememberMe bool `json:"rememberMe,omitempty"`
}

// BruteForcePolicy is what happens after repeated failures.
type BruteForcePolicy struct {
	// Enabled turns detection on. Off is Keycloak's default and is worth
	// stating deliberately: without it, a password can be guessed at the
	// speed of the network.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// MaxLoginFailures before the account is locked out.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MaxLoginFailures int32 `json:"maxLoginFailures,omitempty"`

	// LockoutDurationSeconds is how long a lockout lasts.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=604800
	LockoutDurationSeconds int32 `json:"lockoutDurationSeconds,omitempty"`
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

	// DefaultGrantedApps lists the apps whose default grant
	// (spec.apps[].defaultGrant) has been applied: the tenant's members at
	// that moment were added to the app's group. An app listed here is not
	// granted again, which is what keeps a person removed from the group
	// since then removed. An entry leaves the list when the app is
	// uninstalled or no longer declares the grant, so declaring it again
	// applies it again.
	// +optional
	// +listType=set
	DefaultGrantedApps []string `json:"defaultGrantedApps,omitempty"`

	// AppCount is the total number of apps requested in spec.
	// +optional
	AppCount int `json:"appCount,omitempty"`

	// ReadyApps is the number of apps that have been successfully provisioned.
	// +optional
	ReadyApps int `json:"readyApps,omitempty"`
	// Namespace is the resolved tenant namespace name.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// AdminEmail is the resolved contact address for this tenant, and the
	// administrator's login: admin@<effectiveDomain>.
	//
	// Reported here because it is derived — there is no spec field to read it
	// back from — so a consumer that needs the address reads status, and a
	// tenant whose domain changes shows the new one here once reconciled.
	// +optional
	AdminEmail string `json:"adminEmail,omitempty"`

	// Domain is the custom domain a TenantDomain binds this tenant to, copied
	// here by the operator so every consumer of EffectiveDomain reads it from
	// the tenant itself. Empty when no TenantDomain names this tenant.
	// +optional
	Domain string `json:"domain,omitempty"`
	// ObservedGeneration is the last processed generation of the spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Mail holds the observed DNS record data produced by the mail reconciler.
	// Operators must publish these values in the tenant's DNS zone.
	// +optional
	Mail *TenantMailStatus `json:"mail,omitempty"`

	// ResourcePlan is the plan whose selection was last recorded in the
	// tenant's usage history: the value of the resource-plan annotation at
	// the time the plan event was written. It differing from the annotation
	// is what tells the reconciler a change has landed that the billing
	// record does not know about yet.
	// +optional
	ResourcePlan string `json:"resourcePlan,omitempty"`
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
// +kubebuilder:printcolumn:name="ADMIN",type=string,JSONPath=`.status.adminEmail`
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
// TenantAdminLocalPart is the local part of every tenant administrator's
// address, and of the Keycloak username, which are the same string.
const TenantAdminLocalPart = "admin"

// AdminEmailOrDefault is the tenant administrator's address — and its login.
//
// admin@<tenant-domain>: admin@corp.gtn.host in multi mode, admin@<kernelDomain>
// in single, the custom domain when a TenantDomain binds one. The tenant's own
// domain is what makes `admin` unambiguous, so no tenant name appears in the
// local part; it would name the tenant twice.
//
// Derived, never configured. spec.adminEmail is gone: an address an operator
// could type is an address pointing outside the tenant, and this account is
// recovered by the cluster administrator rather than by mail to a third party.
// Every tenant definition that carried one carried a hand-written value that
// had to be kept in step with the domain, and none of them were.
//
// The address IS the username — see TenantAdminUsername — so there is one
// identifier here, not two that can disagree.
func (t *Tenant) AdminEmailOrDefault(kernelDomain, tenancyMode string) string {
	domain := t.EffectiveDomain(kernelDomain, tenancyMode)
	if domain == "" {
		// No domain configured at all: .invalid is reserved by RFC 2606 and can
		// never resolve, which is the honest representation of "unknown".
		domain = t.Name + ".invalid"
	}
	return TenantAdminLocalPart + "@" + domain
}

// TenantAdminUsername is the Keycloak login for that account.
//
// The same string as the address, deliberately. It was admin-<tenant>, which
// made the login and the contact address two identifiers for one account that
// had to be derived in two places and could disagree — and did: the login
// carried the tenant name while the address did not.
//
// One string also states the recovery model. A login that is an address the
// tenant's own mail stack delivers to is an account whose password reset goes
// to the tenant, not to whoever typed a contact address into a definition once.
func (t *Tenant) TenantAdminUsername(kernelDomain, tenancyMode string) string {
	return t.AdminEmailOrDefault(kernelDomain, tenancyMode)
}

// EffectiveDomain returns the domain to use for ingress and mail routing
// for this tenant: the custom domain a TenantDomain bound it to (status.domain)
// when there is one; otherwise "<tenant-name>.<kernelDomain>" in multi-tenancy
// mode and "<kernelDomain>" in single-tenancy mode (flat app hostnames). An
// empty kernelDomain without a custom domain returns the empty string —
// callers must treat that as a configuration error and skip ingress
// provisioning.
//
// See docs/design/multi-tenancy.md §3.
func (t *Tenant) EffectiveDomain(kernelDomain, tenancyMode string) string {
	if t.Status.Domain != "" {
		return t.Status.Domain
	}
	if kernelDomain == "" {
		return ""
	}
	if NormalizeTenancyMode(tenancyMode) == TenancyModeSingle {
		return kernelDomain
	}
	return t.Name + "." + kernelDomain
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
