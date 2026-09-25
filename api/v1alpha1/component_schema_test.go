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

package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// These tests run the generated CRDs the way the API server would: OpenAPI
// validation first, then the CEL rules, with an old object where a rule is about
// a transition. What they guard is that the rules say what the types' comments
// claim — a CEL rule that never fires is indistinguishable, in review, from one
// that does.

type crdValidator struct {
	schema   *structuralschema.Structural
	openapi  validation.SchemaValidator
	celRules *cel.Validator
}

func loadCRD(t *testing.T, file string) *crdValidator {
	t.Helper()
	raw, err := os.ReadFile("../../config/crd/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var v1crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &v1crd); err != nil {
		t.Fatal(err)
	}
	var crd apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1crd, &crd, nil); err != nil {
		t.Fatal(err)
	}
	// What the API server checks before it accepts the CRD at all — including
	// the CEL cost budget, which a transition rule over an unbounded list or
	// string exceeds. The first version of Component passed every rule test
	// here and was refused by envtest for exactly that.
	// The server fills status on create; the validator expects it present.
	crd.Status.StoredVersions = []string{crd.Spec.Versions[0].Name}
	if errs := apiextvalidation.ValidateCustomResourceDefinition(context.Background(), &crd); len(errs) > 0 {
		t.Fatalf("the API server would refuse %s:\n%v", file, errs.ToAggregate())
	}
	props := crd.Spec.Versions[0].Schema
	if props == nil {
		props = crd.Spec.Validation
	}
	ss, err := structuralschema.NewStructural(props.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	sv, _, err := validation.NewSchemaValidator(props.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	return &crdValidator{schema: ss, openapi: sv, celRules: cel.NewValidator(ss, true, celconfig.PerCallLimit)}
}

// check returns every validation message for obj (and the transition from old,
// when given), joined.
func (v *crdValidator) check(t *testing.T, objYAML, oldYAML string) string {
	t.Helper()
	parse := func(s string) map[string]any {
		if s == "" {
			return nil
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("fixture does not parse: %v\n%s", err, s)
		}
		return m
	}
	obj, old := parse(objYAML), parse(oldYAML)
	var msgs []string
	for _, e := range validation.ValidateCustomResource(field.NewPath(""), obj, v.openapi) {
		msgs = append(msgs, e.Error())
	}
	var oldObj any
	if old != nil {
		oldObj = old
	}
	errs, _ := v.celRules.Validate(context.Background(), field.NewPath(""), v.schema, obj, oldObj, celconfig.RuntimeCELCostBudget)
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}

// celReserved are the words CEL cannot read as a field name. Kubernetes
// escapes them as __word__, and an API server before 1.32 refuses a rule that
// does not — while 1.32 (and this package's own validator) accept it, so
// neither envtest nor the rule tests above can catch one. This does.
var celReserved = []string{
	"as", "break", "const", "continue", "else", "false", "for", "function", "if",
	"import", "in", "let", "loop", "namespace", "null", "package", "return",
	"true", "var", "void", "while",
}

// TestNoRuleReadsAReservedWordUnescaped walks every generated CRD's validation
// rules and refuses self.<reserved>.
func TestNoRuleReadsAReservedWordUnescaped(t *testing.T) {
	files, err := filepath.Glob("../../config/crd/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs: %v", err)
	}
	bad := regexp.MustCompile(`self\.(` + strings.Join(celReserved, "|") + `)\b`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "rule:") && !bad.MatchString(line) {
				continue
			}
			if m := bad.FindStringSubmatch(line); m != nil {
				t.Errorf("%s: a rule reads self.%s, which CEL cannot: write self.__%s__\n  %s",
					filepath.Base(f), m[1], m[1], strings.TrimSpace(line))
			}
		}
	}
}

func expect(t *testing.T, name, got, want string) {
	t.Helper()
	switch {
	case want == "" && got != "":
		t.Errorf("%s: should be admitted, got:\n%s", name, got)
	case want != "" && !strings.Contains(got, want):
		t.Errorf("%s: should be refused with %q, got:\n%s", name, want, got)
	}
}

const profileHead = `
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: x}
spec:
  version: "1.0.0"
  package: {chart: {repository: "oci://example/charts", name: x, version: "1.0.0"}}
`

func TestComponentProfileRules(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")
	// A gateway entry with no tile, so a case that is not about tiles does not
	// have to think about launch beyond saying "none".
	gateway := `
  expose:
  - {name: web, surface: gateway, authMode: oidc, backend: {service: x, port: 80}}
`
	tile := `
  expose:
  - name: web
    surface: gateway
    authMode: oidc
    backend: {service: x, port: 80}
    tile: {displayName: Files, icon: files, relation: can_use}
`
	app := "  classes: [app]\n  launch: none\n  trustTier: certified"
	cases := []struct{ name, spec, want string }{
		{"an app on the gateway", app + gateway, ""},
		{"a service with no console", "  classes: [service]\n  launch: none\n  trustTier: platform\n", ""},
		{"shared-app and app at platform tier", "  classes: [shared-app, app]\n  launch: none\n  trustTier: platform" + gateway, ""},

		{"service is exclusive", "  classes: [service, app]\n  launch: none\n  trustTier: platform\n", "service is exclusive"},
		{"shared-app below platform tier", "  classes: [shared-app]\n  launch: none\n  trustTier: certified\n", "shared-app requires trustTier platform"},
		{"no class at all", "  classes: []\n  launch: none\n  trustTier: certified\n", "classes"},
		{"an unknown class", "  classes: [global]\n  launch: none\n  trustTier: certified\n", "Unsupported value"},
		{"the old vocabulary", "  classes: [tenant]\n  launch: none\n  trustTier: certified\n", "Unsupported value"},
		{"no trust tier", "  classes: [app]\n  launch: none\n", "trustTier"},

		// A service may have a console. What it may not have is a console on
		// the perimeter, or a tile asking about an object it does not have.
		{"a service with a console", "  classes: [service]\n  launch: tile\n  trustTier: platform\n  expose:\n  - {name: console, surface: gateway, authMode: oidc, backend: {service: x, port: 80}, tile: {displayName: Models, icon: model, relation: can_operate_system, object: cluster}}\n", ""},
		{"a service on the perimeter", "  classes: [service]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: hook, surface: perimeter, authMode: signature, backend: {service: x, port: 80}}\n", "a service exposes on the gateway only"},
		{"a service tile asking about an app", "  classes: [service]\n  launch: tile\n  trustTier: platform\n  expose:\n  - {name: console, surface: gateway, authMode: oidc, backend: {service: x, port: 80}, tile: {displayName: Models, icon: model, relation: can_use, object: app}}\n", "a service's tile asks on the cluster"},
		{"a service given to every tenant", "  classes: [service]\n  launch: none\n  trustTier: platform\n  defaultForTenants: true\n", "defaultForTenants is for class app"},

		// Launch: a person must be able to reach what they installed.
		{"launch is not optional", "  classes: [app]\n  trustTier: certified" + gateway, "launch"},
		{"a tile, and launch says so", "  classes: [app]\n  launch: tile\n  trustTier: certified" + tile, ""},
		{"launch tile with no tile", "  classes: [app]\n  launch: tile\n  trustTier: certified" + gateway, "launch tile needs a tile"},
		{"a tile the profile does not own up to", "  classes: [app]\n  launch: none\n  trustTier: certified" + tile, "a tile means launch tile"},
		{"opened by another component", "  classes: [app]\n  launch: from\n  launchFrom: file-store\n  trustTier: certified" + gateway, ""},
		{"launch from with nothing named", "  classes: [app]\n  launch: from\n  trustTier: certified" + gateway, "launch from needs launchFrom"},
		{"launchFrom without launch from", "  classes: [app]\n  launch: none\n  launchFrom: file-store\n  trustTier: certified" + gateway, "launchFrom is meaningless"},

		{"authMode has no default", app + "\n  expose:\n  - {name: web, surface: gateway, backend: {service: x, port: 80}}\n", "authMode"},
		{"surface has no default", app + "\n  expose:\n  - {name: web, authMode: oidc, backend: {service: x, port: 80}}\n", "surface"},
		{"an authMode that is not a mode", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: forward-bearer, backend: {service: x, port: 80}}\n", "Unsupported value"},
		{"none is a word someone may write", app + "\n  expose:\n  - {name: share, surface: perimeter, authMode: none, paths: [/s/], backend: {service: x, port: 80}}\n", ""},
		{"a perimeter entry has no session", app + "\n  expose:\n  - {name: share, surface: perimeter, authMode: oidc, backend: {service: x, port: 80}}\n", "cannot use authMode oidc"},

		{"forwardToken below platform tier", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, forwardToken: true, backend: {service: x, port: 80}}\n", "forwardToken requires trustTier platform"},
		{"forwardToken at platform tier", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, forwardToken: true, backend: {service: x, port: 80}}\n", ""},
		{"forwardToken on the perimeter", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: hook, surface: perimeter, authMode: signature, forwardToken: true, backend: {service: x, port: 80}}\n", "meaningless on a perimeter entry"},

		{"a pinned caller, by component", app + "\n  expose:\n  - {name: wopi, surface: gateway, authMode: none, source: {component: collabora}, backend: {service: x, port: 80}}\n", ""},
		{"a source that pins nothing", app + "\n  expose:\n  - {name: wopi, surface: gateway, authMode: none, source: {}, backend: {service: x, port: 80}}\n", "exactly one of cidrs or component"},
		{"a source that pins both ways", app + "\n  expose:\n  - {name: wopi, surface: gateway, authMode: none, source: {component: c, cidrs: [10.0.0.0/8]}, backend: {service: x, port: 80}}\n", "exactly one of cidrs or component"},

		// An exposure may route into another component: an addon's tile points
		// into its base, and a bound shared-app routes into shared-<app>.
		{"a backend in another component", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, backend: {component: odoo-base-ce, service: odoo, port: 80}}\n", ""},

		// The package is exactly one thing, and nothing outside it completes
		// the union. The OR this replaces admitted the first two of these.
		{"a package that is nothing", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {}\n", "exactly one of chart, composition, api or addon"},
		{"a chart beside an API integration", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {chart: {repository: oci://r, name: \"nc\", version: \"1\"}, api: {baseUrl: \"https://x\"}}\n", "exactly one of chart, composition, api or addon"},
		{"a package that is an addon", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {addon: {id: deck, of: nextcloud-base-ce}}\n", ""},
		{"a package that is a composition", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {composition: element-stack}\n", ""},
		{"a package that is an API integration", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {api: {baseUrl: \"https://corp.example\"}}\n", ""},
		{"an addon left in customization", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {composition: x}\n  customization: {addon: {id: deck, of: nextcloud-base-ce}}\n", "an addon is package.addon on this kind"},

		{"a privilege without a reason", app + "\n  requires:\n    privileges:\n      podSecurity:\n      - {name: root, policy: require-run-as-nonroot, scope: web}\n", "reason"},
		{"a privilege with one", app + "\n  requires:\n    privileges:\n      podSecurity:\n      - {name: root, policy: require-run-as-nonroot, scope: web, reason: \"the upstream image starts as root and drops privileges itself\"}\n", ""},
		{"requirements are services now", app + "\n  requires:\n    services:\n      database: {engine: postgresql}\n", ""},
		// The old names cannot be tested for: a structural schema prunes an
		// unknown field before CEL runs, so requires.contracts and
		// package.deploymentMethod are accepted and silently dropped. A
		// profile converted without renaming them loses its requirements
		// rather than being refused, which is the converter's job to prevent
		// and scripts/lint/lint-legacy-profile-fields.sh guards.
	}
	for _, c := range cases {
		expect(t, c.name, v.check(t, profileHead+c.spec, ""), c.want)
	}
}

// Every profile the conversion tool produced from the real catalogue must be
// something the API server would admit. Run by scripts/tools/convert-appprofiles.sh.
func TestConvertedProfilesAreAdmitted(t *testing.T) {
	dir := os.Getenv("COMPONENT_PROFILE_DIR")
	if dir == "" {
		t.Skip("set COMPONENT_PROFILE_DIR to a directory of converted profiles")
	}
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		n++
		expect(t, e.Name(), v.check(t, string(raw), ""), "")
	}
	if n == 0 {
		t.Fatalf("no profiles in %s", dir)
	}
	if !t.Failed() {
		t.Logf("all %d converted profiles are admitted", n)
	}
}

func component(spec string) string {
	return "apiVersion: gentianos.io/v1alpha1\nkind: Component\nmetadata: {name: nextcloud, namespace: tenant-demo}\nspec:\n  profileRef: {name: nextcloud}\n" + spec
}

func TestComponentRules(t *testing.T) {
	v := loadCRD(t, "gentianos.io_components.yaml")
	enabled := func(owner, expires string) string {
		s := "  class: app\n  exposures:\n  - exposureName: share\n    owner: " + owner + "\n"
		if expires != "" {
			s += "    expiresAt: \"" + expires + "\"\n"
		}
		return s
	}
	granted := func(approver string) string {
		return "  class: app\n  privileges:\n  - privilege: egress/smtp-relay\n    approver: " + approver +
			"\n    approvedAt: \"2026-09-01T00:00:00Z\"\n    reason: \"relay for the customer's own mail domain\"\n"
	}
	cases := []struct{ name, obj, old, want string }{
		{"a plain tenant install", component("  class: app\n"), "", ""},
		{"pinned to a dedicated backend", component("  class: app\n  fulfilment: dedicated\n"), "", ""},
		{"no tenant can demand a shared backend", component("  class: app\n  fulfilment: shared\n"), "", "Unsupported value"},
		{"fulfilment is a tenant's choice", component("  class: shared-app\n  fulfilment: dedicated\n"), "", "applies to class app only"},

		{"an exposure with an end", component(enabled("u-pat", "2026-12-01T00:00:00Z")), "", ""},
		{"an exposure without one", component(enabled("u-pat", "")), "", "expiresAt"},
		{"an exposure nobody owns", component("  class: app\n  exposures:\n  - {exposureName: share, expiresAt: \"2026-12-01T00:00:00Z\"}\n"), "", "owner"},
		{"review after expiry", component(enabled("u-pat", "2026-12-01T00:00:00Z") + "    reviewAt: \"2027-01-01T00:00:00Z\"\n"), "", "reviewAt must not be later"},
		{"a service on the perimeter", component("  class: service\n  exposures:\n  - {exposureName: share, owner: u, expiresAt: \"2026-12-01T00:00:00Z\"}\n"), "", "a service switches on no perimeter entry"},
		{"a vanity host", component(enabled("u-pat", "2026-12-01T00:00:00Z") + "    host: www.example.org\n"), "", ""},
		{"a host that is not one", component(enabled("u-pat", "2026-12-01T00:00:00Z") + "    host: \"*.example.org\"\n"), "", "host"},

		{"renewed by its owner", component(enabled("u-pat", "2027-03-01T00:00:00Z")), component(enabled("u-pat", "2026-12-01T00:00:00Z")), ""},
		{"taken over by someone else", component(enabled("u-mallory", "2027-03-01T00:00:00Z")), component(enabled("u-pat", "2026-12-01T00:00:00Z")), "owner is immutable"},
		{"a grant rewritten to another approver", component(granted("u-mallory")), component(granted("u-sam")), "approver and approvedAt are immutable"},
		{"a grant for something that is not a privilege", component("  class: app\n  privileges:\n  - {privilege: root, approver: u, approvedAt: \"2026-09-01T00:00:00Z\", reason: \"because it was asked for\"}\n"), "", "privilege"},
		{"a grant without the approver's reason", component("  class: app\n  privileges:\n  - {privilege: egress/x, approver: u, approvedAt: \"2026-09-01T00:00:00Z\", reason: ok}\n"), "", "reason"},

		{"class changed in place", component("  class: shared-app\n"), component("  class: app\n"), "class is immutable"},
		{"pointed at another profile", strings.Replace(component("  class: app\n"), "{name: nextcloud}", "{name: odoo}", 1), component("  class: app\n"), "profileRef.name is immutable"},
	}
	for _, c := range cases {
		expect(t, c.name, v.check(t, c.obj, c.old), c.want)
	}
}
