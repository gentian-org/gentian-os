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

// Types in this file describe the customization surface of a catalogue app —
// which rungs of the Gentian customization ladder the app supports, and by which
// mechanism. They are deliberately app-neutral: no per-app fields, no profile
// names. See docs/app-customization.md.

// CustomizationRung identifies a rung of the customization ladder.
// Rungs are ordered by how much of the app's own delivery artifact Gentian owns,
// which is what determines upgrade cost.
// +kubebuilder:validation:Enum=L0;L1;L2;L3;L4;L5;L6
type CustomizationRung string

const (
	// RungConfigure sets a value the app already exposes. Nothing new is owned.
	RungConfigure CustomizationRung = "L0"
	// RungDropIn adds a file to a directory the app already treats as an extension point.
	RungDropIn CustomizationRung = "L1"
	// RungCompanion builds a separate deployable against the app's published API.
	// Never appears in SupportedRungs — it is a property of the customization, not
	// of the target, and is therefore always available.
	RungCompanion CustomizationRung = "L2"
	// RungExtension loads a module through the app's own extension system.
	RungExtension CustomizationRung = "L3"
	// RungRepackage owns the packaging (chart, composition, entrypoint); source untouched.
	RungRepackage CustomizationRung = "L4"
	// RungPatch carries a quilt-style patch series over pinned upstream source.
	RungPatch CustomizationRung = "L5"
	// RungFork owns a source tree with its own release train.
	RungFork CustomizationRung = "L6"
)

// CustomizationGrade rates how far up the ladder an app can be customized.
// Assigned by hand by the catalogue maintainer for v0.4; see roadmap item 2.13.
// +kubebuilder:validation:Enum=A;B;C;D;unknown
type CustomizationGrade string

const (
	// GradeA has a documented, versioned plugin API, declared drop-in dirs and a
	// published API for companions. Rungs L0-L3 are reachable.
	GradeA CustomizationGrade = "A"
	// GradeB has config, drop-ins and a published API but no plugin system.
	GradeB CustomizationGrade = "B"
	// GradeC is config-only and monolithic.
	GradeC CustomizationGrade = "C"
	// GradeD requires touching source for anything beyond a value change.
	GradeD CustomizationGrade = "D"
	// GradeUnknown means the app has not been characterised yet. Agents must not
	// infer more than the L0/L4 default.
	GradeUnknown CustomizationGrade = "unknown"
)

// CustomizationSurface declares the customization ladder for one catalogue app.
//
// When absent, consumers must assume {Grade: unknown, SupportedRungs: [L0, L4]}
// and raise a task to characterise the app. L2 is always available regardless.
type CustomizationSurface struct {
	// Grade rates customization readiness (see CustomizationGrade). It summarises
	// the rubric score recorded in the profile's customization.md.
	// +optional
	// +kubebuilder:default=unknown
	Grade CustomizationGrade `json:"grade,omitempty"`

	// RubricScore is the §4.1 rubric total backing Grade (0-8). CI checks that the
	// score and the grade agree: A>=7, B 5-6, C 3-4, D<=2.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	RubricScore *int32 `json:"rubricScore,omitempty"`

	// SupportedRungs lists the rungs at which this app can be customized.
	// L2 must not be listed: a companion app is always buildable, so L2 is a
	// property of the customization rather than of the target.
	// +optional
	// +listType=set
	SupportedRungs []CustomizationRung `json:"supportedRungs,omitempty"`

	// LadderDocs links to human documentation of this app's ladder.
	// +optional
	LadderDocs string `json:"ladderDocs,omitempty"`

	// Configure describes the L0 surface.
	// +optional
	Configure *CustomizationConfigure `json:"configure,omitempty"`

	// DropIns declares the L1 surface: directories the app treats as extension points.
	// +optional
	// +listType=map
	// +listMapKey=name
	DropIns []CustomizationDropIn `json:"dropIns,omitempty"`

	// Extension describes the L3 surface: the app's own module system.
	// +optional
	Extension *CustomizationExtension `json:"extension,omitempty"`

	// Publishes describes what this app offers so that *another* app can be built
	// at L2 against it. It is not a rung of this app.
	// +optional
	Publishes *CustomizationPublishes `json:"publishes,omitempty"`

	// Repackage describes the L4 surface: who owns the packaging.
	// +optional
	Repackage *CustomizationRepackage `json:"repackage,omitempty"`

	// Patch describes the L5 surface: whether a patch series is permitted.
	// +optional
	Patch *CustomizationPatch `json:"patch,omitempty"`

	// Fork describes the L6 surface: whether a fork is permitted, and who owns it.
	// +optional
	Fork *CustomizationFork `json:"fork,omitempty"`
}

// CustomizationConfigure describes the L0 (values) surface.
type CustomizationConfigure struct {
	// ValuesSchema is the repo-relative path to the chart's values.schema.json.
	// Its presence is what makes L0 machine-checkable.
	// +optional
	ValuesSchema string `json:"valuesSchema,omitempty"`

	// HotReload reports whether a values change applies without a restart.
	// +optional
	HotReload bool `json:"hotReload,omitempty"`
}

// CustomizationDropInFormat is the content format of a drop-in directory.
// +kubebuilder:validation:Enum=ini;yaml;json;toml;xml;properties;files
type CustomizationDropInFormat string

// CustomizationDropInSource is how drop-in content reaches the pod.
// +kubebuilder:validation:Enum=configMap;secret
type CustomizationDropInSource string

// CustomizationDropIn declares one L1 drop-in directory.
type CustomizationDropIn struct {
	// Name identifies the drop-in. Tenant drop-ins reference it by this name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Path is the absolute in-container directory the files are mounted at.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/.*`
	Path string `json:"path"`

	// Format is the content format expected in this directory. Content is parsed
	// at admission so a malformed fragment fails there rather than at pod boot.
	// +kubebuilder:validation:Required
	Format CustomizationDropInFormat `json:"format"`

	// Source is the Kubernetes object the content is delivered through.
	// +optional
	// +kubebuilder:default=configMap
	Source CustomizationDropInSource `json:"source,omitempty"`

	// UpstreamDocumented reports whether upstream documents this path as an
	// extension point. When false the mount is really L4, not L1: upstream has
	// made no promise about the path surviving a reorganisation.
	// +optional
	UpstreamDocumented bool `json:"upstreamDocumented,omitempty"`

	// TenantEditable allows tenant admins to supply files here through
	// Tenant.spec.apps[].config.dropIns. Not every drop-in directory is safe to
	// expose — a policy file usually is not.
	// +optional
	TenantEditable bool `json:"tenantEditable,omitempty"`

	// MaxBytes caps the total tenant-supplied content for this drop-in.
	// +optional
	// +kubebuilder:default=262144
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	MaxBytes int32 `json:"maxBytes,omitempty"`
}

// CustomizationAPIStability rates the stability of an app's extension API.
// +kubebuilder:validation:Enum=stable;evolving;undocumented;none
type CustomizationAPIStability string

const (
	// APIStabilityStable means the extension API is versioned with a deprecation policy.
	APIStabilityStable CustomizationAPIStability = "stable"
	// APIStabilityEvolving means the API exists and is documented but may break.
	APIStabilityEvolving CustomizationAPIStability = "evolving"
	// APIStabilityUndocumented means extension is possible but nothing is promised.
	APIStabilityUndocumented CustomizationAPIStability = "undocumented"
	// APIStabilityNone means the app has no extension system.
	APIStabilityNone CustomizationAPIStability = "none"
)

// CustomizationDelivery is how an L3 module reaches the running app.
// +kubebuilder:validation:Enum=git-sidecar;image-layer;module-profile;app-store-api
type CustomizationDelivery string

const (
	// DeliveryGitSidecar syncs a git repo into the app's module path.
	DeliveryGitSidecar CustomizationDelivery = "git-sidecar"
	// DeliveryImageLayer bakes modules into a Gentian-built image layer.
	DeliveryImageLayer CustomizationDelivery = "image-layer"
	// DeliveryModuleProfile ships the module as a catalogue-visible AppProfile.
	DeliveryModuleProfile CustomizationDelivery = "module-profile"
	// DeliveryAppStoreAPI installs through the app's own runtime admin API.
	DeliveryAppStoreAPI CustomizationDelivery = "app-store-api"
)

// CustomizationExtension describes the L3 (in-app module) surface.
type CustomizationExtension struct {
	// Mechanism names the app's extension system (for example "odoo-addon",
	// "nextcloud-app", "xwiki-extension", "activepieces-piece").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	Mechanism string `json:"mechanism"`

	// Delivery lists the supported ways a module reaches the running app.
	// +optional
	// +listType=set
	Delivery []CustomizationDelivery `json:"delivery,omitempty"`

	// Registry is the repository or index modules are sourced from.
	// +optional
	Registry string `json:"registry,omitempty"`

	// ModulePath is the in-container directory modules are loaded from.
	// +optional
	// +kubebuilder:validation:Pattern=`^/.*`
	ModulePath string `json:"modulePath,omitempty"`

	// APIStability rates the extension API. When this is not "stable", the L2/L3
	// tie-break in the decision procedure defaults to L2.
	// +optional
	// +kubebuilder:default=undocumented
	APIStability CustomizationAPIStability `json:"apiStability,omitempty"`

	// APIDocs links to the extension API documentation.
	// +optional
	APIDocs string `json:"apiDocs,omitempty"`

	// PerTenantModules reports whether tenants may have divergent module sets.
	// Only valid when the app runs in the tenant's own namespace — a shared
	// runtime with per-tenant code is a cross-tenant data leak waiting to happen.
	// +optional
	PerTenantModules bool `json:"perTenantModules,omitempty"`

	// TestMatrix lists the upstream app versions modules are tested against.
	// +optional
	// +listType=set
	TestMatrix []string `json:"testMatrix,omitempty"`
}

// CustomizationPublishes describes what this app offers to L2 companions.
type CustomizationPublishes struct {
	// APIs lists the published interfaces a companion app can build against.
	// +optional
	// +listType=map
	// +listMapKey=contract
	APIs []CustomizationPublishedAPI `json:"apis,omitempty"`
}

// CustomizationPublishedAPI is one interface offered to companion apps.
type CustomizationPublishedAPI struct {
	// Contract is the Gentian contract name this API satisfies.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	Contract string `json:"contract"`

	// Protocol is the wire protocol (for example "http-json", "webdav", "matrix").
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// Spec names the interface description format (for example "openapi", "graphql").
	// +optional
	Spec string `json:"spec,omitempty"`

	// Path is the base path the API is served at.
	// +optional
	Path string `json:"path,omitempty"`

	// Auth is the authentication method companions must use.
	// +optional
	Auth string `json:"auth,omitempty"`
}

// CustomizationChartOwnership records who owns the packaging of this app.
// +kubebuilder:validation:Enum=upstream;gentian-owned;vendored
type CustomizationChartOwnership string

// CustomizationRepackage describes the L4 (packaging) surface.
type CustomizationRepackage struct {
	// ChartOwnership records whether the chart is upstream's, Gentian-authored, or
	// a vendored copy. A vendored chart is a fork of the packaging and needs an
	// UPSTREAM.md.
	// +optional
	// +kubebuilder:default=upstream
	ChartOwnership CustomizationChartOwnership `json:"chartOwnership,omitempty"`

	// CompositionRef names the profile-scoped Crossplane composition, when the app
	// needs a custom managed-resource graph.
	// +optional
	CompositionRef string `json:"compositionRef,omitempty"`
}

// CustomizationPatch describes the L5 (patch series) surface.
type CustomizationPatch struct {
	// Allowed gates whether a patch series may be carried for this app at all.
	// +optional
	Allowed bool `json:"allowed,omitempty"`

	// BuildRepo is the repository holding the pinned upstream and the series.
	// +optional
	BuildRepo string `json:"buildRepo,omitempty"`

	// SeriesPath is the repo-relative path to the quilt-style series file.
	// +optional
	// +kubebuilder:default=patches/series
	SeriesPath string `json:"seriesPath,omitempty"`

	// RequiresApproval names the role that must approve a new patch.
	// +optional
	// +kubebuilder:default=platform-team
	RequiresApproval string `json:"requiresApproval,omitempty"`
}

// CustomizationFork describes the L6 (fork) surface.
type CustomizationFork struct {
	// Allowed gates whether a fork is a permitted outcome for this app.
	// +optional
	Allowed bool `json:"allowed,omitempty"`

	// Repo is the Gentian-owned fork.
	// +optional
	Repo string `json:"repo,omitempty"`

	// Upstream is the project the fork diverged from.
	// +optional
	Upstream string `json:"upstream,omitempty"`

	// Owner is the team or organisation accountable for the fork, including its
	// CVE response. Free-form so an external maintainer can hold it later.
	// +optional
	Owner string `json:"owner,omitempty"`

	// CVEWatch reports whether the fork has active vulnerability monitoring.
	// A fork without it is an unowned liability.
	// +optional
	CVEWatch bool `json:"cveWatch,omitempty"`
}
