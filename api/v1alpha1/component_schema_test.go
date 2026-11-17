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

// tokenEntry is a profile of platform trust with one token entry, and
// whatever else the entry is to say.
func tokenEntry(more string) string {
	return "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n" +
		"  - {name: mcp, surface: gateway, authMode: bearer, subDomain: mcp, backend: {service: x, port: 80}" + more + "}\n"
}

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
    tile: {displayName: Files, logo: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQU5v2HwADOgHiG7pP+wAAAABJRU5ErkJggg==", relation: can_use}
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
		{"a service with a console", "  classes: [service]\n  launch: tile\n  trustTier: platform\n  expose:\n  - {name: console, surface: gateway, authMode: oidc, backend: {service: x, port: 80}, tile: {displayName: Models, logo: \"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQU5v2HwADOgHiG7pP+wAAAABJRU5ErkJggg==\", relation: can_operate_system, object: cluster}}\n", ""},
		{"a service on the perimeter", "  classes: [service]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: hook, surface: perimeter, authMode: none, backend: {service: x, port: 80}}\n", "a service exposes on the gateway only"},
		{"a service tile asking about an app", "  classes: [service]\n  launch: tile\n  trustTier: platform\n  expose:\n  - {name: console, surface: gateway, authMode: oidc, backend: {service: x, port: 80}, tile: {displayName: Models, logo: \"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQU5v2HwADOgHiG7pP+wAAAABJRU5ErkJggg==\", relation: can_use, object: app}}\n", "a service's tile asks on the cluster"},
		{"a service given to every tenant", "  classes: [service]\n  launch: none\n  trustTier: platform\n  defaultForTenants: true\n", "defaultForTenants is for class app"},

		// Launch: a person must be able to reach what they installed.
		{"launch is not optional", "  classes: [app]\n  trustTier: certified" + gateway, "launch"},
		{"a tile, and launch says so", "  classes: [app]\n  launch: tile\n  trustTier: certified" + tile, ""},
		{"launch tile with no tile", "  classes: [app]\n  launch: tile\n  trustTier: certified" + gateway, "launch tile needs a tile"},
		{"a tile the profile does not own up to", "  classes: [app]\n  launch: none\n  trustTier: certified" + tile, "a tile means launch tile"},
		// A tile is a PNG. An SVG is a document a browser runs, and a PNG
		// media type over bytes that are not one is not a PNG either.
		{"an SVG tile", "  classes: [app]\n  launch: tile\n  trustTier: certified" + strings.Replace(tile, "data:image/png;base64,iVBORw0KGgo", "data:image/svg+xml;base64,PHN2Zy8+", 1), "logo"},
		{"a PNG media type over an SVG", "  classes: [app]\n  launch: tile\n  trustTier: certified" + strings.Replace(tile, "iVBORw0KGgo", "PHN2Zy8+", 1), "logo"},
		{"opened by another component", "  classes: [app]\n  launch: from\n  trustTier: certified" + gateway, ""},

		{"authMode has no default", app + "\n  expose:\n  - {name: web, surface: gateway, backend: {service: x, port: 80}}\n", "authMode"},
		{"surface has no default", app + "\n  expose:\n  - {name: web, authMode: oidc, backend: {service: x, port: 80}}\n", "surface"},
		{"an authMode that is not a mode", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: forward-bearer, backend: {service: x, port: 80}}\n", "Unsupported value"},
		{"none is a word someone may write", app + "\n  expose:\n  - {name: share, surface: perimeter, authMode: none, paths: [/s/], backend: {service: x, port: 80}}\n", ""},
		{"a perimeter entry has no session", app + "\n  expose:\n  - {name: share, surface: perimeter, authMode: oidc, backend: {service: x, port: 80}}\n", "cannot use authMode oidc"},

		{"forwardToken below platform tier", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, forwardToken: true, backend: {service: x, port: 80}}\n", "forwardToken requires trustTier platform"},
		{"forwardToken at platform tier", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, forwardToken: true, backend: {service: x, port: 80}}\n", ""},
		{"forwardToken on the perimeter", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: hook, surface: perimeter, authMode: none, forwardToken: true, backend: {service: x, port: 80}}\n", "meaningless on a perimeter entry"},

		// The two entry types an approver decides beside a plain public
		// address, and the modes that promised a check nothing makes.
		{"a public entry whose callers the app checks", app + "\n  expose:\n  - {name: dav, surface: perimeter, authMode: app, paths: [/dav/], backend: {service: x, port: 80}}\n", ""},
		{"authMode app behind sign-in", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: app, backend: {service: x, port: 80}}\n", "authMode app is for a perimeter entry"},
		{"a password the proxy would have to check", app + "\n  expose:\n  - {name: dav, surface: perimeter, authMode: basic, backend: {service: x, port: 80}}\n", "not available on a perimeter entry yet"},
		{"a token the edge would have to verify", app + "\n  expose:\n  - {name: api, surface: perimeter, authMode: jwt, backend: {service: x, port: 80}}\n", "not available on a perimeter entry yet"},
		{"a bearer the edge would have to verify", app + "\n  expose:\n  - {name: api, surface: perimeter, authMode: bearer, backend: {service: x, port: 80}}\n", "not available on a perimeter entry yet"},
		{"a webhook signature the proxy would have to verify", app + "\n  expose:\n  - {name: hook, surface: perimeter, authMode: signature, backend: {service: x, port: 80}}\n", "Declare authMode app if the app itself checks the credential"},
		{"the main address passes no credential", app + "\n  expose:\n  - {name: site, surface: perimeter, authMode: app, apex: true, paths: [/], backend: {service: x, port: 80}}\n", "an apex entry is a website for anyone"},
		{"a website for the main address", app + "\n  expose:\n  - {name: site, surface: perimeter, authMode: none, apex: true, paths: [/], backend: {service: x, port: 80}}\n", ""},

		{"the Authorization header is the app's own", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, clientAuthorization: app, backend: {service: x, port: 80}}\n", ""},
		{"clientAuthorization is one word", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, clientAuthorization: platform, backend: {service: x, port: 80}}\n", "Unsupported value"},
		{"clientAuthorization on a public entry", app + "\n  expose:\n  - {name: dav, surface: perimeter, authMode: app, clientAuthorization: app, backend: {service: x, port: 80}}\n", "clientAuthorization is for a gateway entry behind the session"},
		{"clientAuthorization without a session", app + "\n  expose:\n  - {name: wopi, surface: gateway, authMode: none, clientAuthorization: app, backend: {service: x, port: 80}}\n", "clientAuthorization is for a gateway entry behind the session"},
		{"the app's own header and the platform's token in one", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, clientAuthorization: app, forwardToken: true, backend: {service: x, port: 80}}\n", "clientAuthorization excludes forwardToken and exchangeToken: the Authorization header"},
		{"the app's own header and an exchanged token in one", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, clientAuthorization: app, exchangeToken: true, backend: {service: x, port: 80}}\n", "clientAuthorization excludes forwardToken and exchangeToken: the Authorization header"},
		{"the app's own header beside an entry that asks for an exchanged token", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, clientAuthorization: app, backend: {service: x, port: 80}}\n  - {name: api, surface: gateway, authMode: oidc, subDomain: api, exchangeToken: true, backend: {service: x, port: 80}}\n", "across a component's entries"},
		{"the app's own header beside an entry that forwards the platform's token", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, clientAuthorization: app, backend: {service: x, port: 80}}\n  - {name: api, surface: gateway, authMode: oidc, subDomain: api, forwardToken: true, backend: {service: x, port: 80}}\n", "excludes forwardToken and exchangeToken across a component's entries"},
		{"a service asks no tenant's approver", "  classes: [service]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: console, surface: gateway, authMode: oidc, clientAuthorization: app, backend: {service: x, port: 80}}\n", "a service does not declare clientAuthorization"},

		// A gateway entry is served only behind the session. The pinned
		// caller (source) that once stood in for it was never routed, and
		// the API server prunes the field, so an entry declaring it is an
		// entry with no session.
		{"a gateway entry that pins its caller instead", app + "\n  expose:\n  - {name: wopi, surface: gateway, authMode: none, source: {component: collabora}, backend: {service: x, port: 80}}\n", "behind the zone's session"},
		{"a gateway entry with no session", app + "\n  expose:\n  - {name: open, surface: gateway, authMode: none, backend: {service: x, port: 80}}\n", "behind the zone's session"},
		// A token entry: a gateway entry that takes a token of the tenant's
		// realm in place of a session, on a host of its own, of a profile of
		// platform trust.
		{"a token entry", tokenEntry(""), ""},
		{"a token entry beside an entry behind sign-in", tokenEntry("") + "  - {name: web, surface: gateway, authMode: oidc, backend: {service: x, port: 80}}\n", ""},
		{"a token entry of a profile that is not of platform trust", app + "\n  expose:\n  - {name: api, surface: gateway, authMode: bearer, subDomain: api, backend: {service: x, port: 80}}\n", "requires trustTier platform"},
		{"a token entry on the component's own host", strings.Replace(tokenEntry(""), "subDomain: mcp, ", "", 1), "names a subDomain"},
		{"a token entry that forwards a token", tokenEntry(", forwardToken: true"), "declares no forwardToken, exchangeToken or tile"},
		{"a token entry that asks for an exchanged token", tokenEntry(", exchangeToken: true"), "declares no forwardToken, exchangeToken or tile"},
		{"a token entry with a tile", strings.Replace(tokenEntry(", tile: {displayName: MCP, relation: can_use, object: app}"), "launch: none", "launch: tile", 1), "declares no forwardToken, exchangeToken or tile"},
		{"a token entry that keeps the app's header", tokenEntry(", clientAuthorization: app"), "clientAuthorization is for a gateway entry behind the session"},
		{"a token entry with the addresses a program returns to", tokenEntry(`, clientRedirectURIs: ["https://claude.ai/api/mcp/auth_callback", "http://localhost/callback", "http://127.0.0.1:8123/callback"]`), ""},
		{"a wildcard return address", tokenEntry(`, clientRedirectURIs: ["https://example.org/*"]`), "clientRedirectURIs"},
		{"a wildcard return host", tokenEntry(`, clientRedirectURIs: ["https://*.example.org/cb"]`), "clientRedirectURIs"},
		{"a return address in the clear that is not the person's own computer", tokenEntry(`, clientRedirectURIs: ["http://example.org/callback"]`), "clientRedirectURIs"},
		{"a return address of a scheme of the program's own", tokenEntry(`, clientRedirectURIs: ["cursor://callback"]`), "clientRedirectURIs"},
		{"return addresses on an entry behind sign-in", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, backend: {service: x, port: 80}, clientRedirectURIs: [\"https://example.org/cb\"]}\n", "clientRedirectURIs is for a token entry"},
		{"a token entry whose tokens a vouching component exchanges", tokenEntry(`, exchangedBy: [chaperone-ee]`), ""},
		{"a component that exchanges and names no keys", "  classes: [app]\n  launch: none\n  trustTier: platform\n  requires:\n    services:\n      vouching: {exchange: true}\n", ""},
		{"a component that vouches and names no keys", "  classes: [app]\n  launch: none\n  trustTier: platform\n  requires:\n    services:\n      vouching: {}\n", "vouching.keys is required"},
		{"a component that asks for the director and names no keys", "  classes: [app]\n  launch: none\n  trustTier: platform\n  requires:\n    services:\n      vouching: {exchange: true, director: [plans-read]}\n", "vouching.director needs vouching.keys"},
		{"exchangers on an entry behind sign-in", "  classes: [app]\n  launch: none\n  trustTier: platform\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, backend: {service: x, port: 80}, exchangedBy: [chaperone-ee]}\n", "exchangedBy is for a token entry"},
		{"two token entries", tokenEntry("") + "  - {name: mcp2, surface: gateway, authMode: bearer, subDomain: mcp2, backend: {service: x, port: 80}}\n", "at most one token entry"},
		{"a token entry of a service", strings.Replace(tokenEntry(""), "classes: [app]", "classes: [service]", 1), "a token entry is for class app"},
		{"a gateway entry with a signed token and no session", app + "\n  expose:\n  - {name: api, surface: gateway, authMode: jwt, backend: {service: x, port: 80}}\n", "behind the zone's session"},

		// An exposure may route into another component: an addon's tile points
		// into its base, and a bound shared-app routes into shared-<app>.
		{"a backend in another component", app + "\n  expose:\n  - {name: web, surface: gateway, authMode: oidc, backend: {component: odoo-base-ce, service: odoo, port: 80}}\n", ""},

		// The package is exactly one thing, and nothing outside it completes
		// the union. The OR this replaces admitted the first two of these.
		{"a package that is nothing", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {}\n", "a chart, a composition, the two together"},
		{"a chart beside an API integration", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {chart: {repository: oci://r, name: \"nc\", version: \"1\"}, api: {baseUrl: \"https://x\"}}\n", "a chart, a composition, the two together"},
		// The one legal pair: they answer different halves of one question.
		// The chart is what is installed; the Composition is what else is
		// created beside it, which is how Odoo gets its portal bridge and
		// Element its SSO sidecar.
		{"a chart rendered by its own composition", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {chart: {repository: oci://r, name: \"nc\", version: \"1\"}, composition: app-odoo-base-ce}\n", ""},
		{"a composition alone", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {composition: app-thing}\n", ""},
		{"an addon beside a chart", "  classes: [app]\n  launch: none\n  trustTier: certified\n  package: {chart: {repository: oci://r, name: \"nc\", version: \"1\"}, addon: {id: cal, of: base}}\n", "a chart, a composition, the two together"},
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

	// A link to a service hosted elsewhere: no chart, no exposure, and the
	// tile on package.api. Refused outright until the launch rules counted it.
	linkHead := `
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: x}
spec:
  version: "1.0.0"
  classes: [app]
  trustTier: experimental
`
	apiTile := `
  package:
    api:
      runtime: redirect
      baseUrl: "https://example.com"
      tile: {displayName: Billing, logo: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQU5v2HwADOgHiG7pP+wAAAABJRU5ErkJggg==", relation: can_administer, object: tenant}
`
	apiNoTile := `
  package:
    api: {runtime: redirect, baseUrl: "https://example.com"}
`
	for _, c := range []struct{ name, spec, want string }{
		{"a link tile, and launch says so", "  launch: tile" + apiTile, ""},
		{"a link tile the profile does not own up to", "  launch: none" + apiTile, "a tile means launch tile"},
		{"launch tile on a link with no tile", "  launch: tile" + apiNoTile, "launch tile needs a tile"},
	} {
		expect(t, c.name, v.check(t, linkHead+c.spec, ""), c.want)
	}
}

// Every profile the conversion tool produced from the real catalog must be
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
	// reviewed is an enablement with its review date, and an expiry when one
	// is given: what is public is always looked at again and ends only if
	// somebody said so.
	reviewed := func(owner, review, expires string) string {
		s := "  class: app\n  exposures:\n  - exposureName: share\n    owner: " + owner + "\n"
		if review != "" {
			s += "    reviewAt: \"" + review + "\"\n"
		}
		if expires != "" {
			s += "    expiresAt: \"" + expires + "\"\n"
		}
		return s
	}
	enabled := func(owner, expires string) string {
		return reviewed(owner, "2026-11-01T00:00:00Z", expires)
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
		{"an exposure meant to stay", component(enabled("u-pat", "")), "", ""},
		{"an approval that says its kind", component(enabled("u-pat", "") + "    kind: signInAppAuthorization\n"), "", ""},
		{"an approval of a public address that passes the credential", component(enabled("u-pat", "") + "    kind: publicAppCredential\n"), "", ""},
		{"an approval of a kind there is not", component(enabled("u-pat", "") + "    kind: everything\n"), "", "Unsupported value"},
		{"an exposure nobody will look at again", component(reviewed("u-pat", "", "2026-12-01T00:00:00Z")), "", "reviewAt"},
		{"an exposure nobody owns", component("  class: app\n  exposures:\n  - {exposureName: share, expiresAt: \"2026-12-01T00:00:00Z\"}\n"), "", "owner"},
		{"review after expiry", component(reviewed("u-pat", "2027-01-01T00:00:00Z", "2026-12-01T00:00:00Z")), "", "reviewAt must not be later"},
		{"a service on the perimeter", component("  class: service\n  exposures:\n  - {exposureName: share, owner: u, expiresAt: \"2026-12-01T00:00:00Z\"}\n"), "", "a service switches on no perimeter entry"},

		{"renewed by its owner", component(enabled("u-pat", "2027-03-01T00:00:00Z")), component(enabled("u-pat", "2026-12-01T00:00:00Z")), ""},
		{"taken over by someone else", component(enabled("u-mallory", "2027-03-01T00:00:00Z")), component(enabled("u-pat", "2026-12-01T00:00:00Z")), "owner is immutable"},
		{"a grant rewritten to another approver", component(granted("u-mallory")), component(granted("u-sam")), "approver and approvedAt are immutable"},
		{"a grant for something that is not a privilege", component("  class: app\n  privileges:\n  - {privilege: root, approver: u, approvedAt: \"2026-09-01T00:00:00Z\", reason: \"because it was asked for\"}\n"), "", "privilege"},
		{"a grant without the approver's reason", component("  class: app\n  privileges:\n  - {privilege: egress/x, approver: u, approvedAt: \"2026-09-01T00:00:00Z\", reason: ok}\n"), "", "reason"},

		{"class changed in place", component("  class: shared-app\n"), component("  class: app\n"), "class is immutable"},
		// The one carve-out, and it is not a loosening: a Component with no
		// class was never creatable, so the only way to hold one is to have
		// been written before spec.tenancy became spec.class. Refusing the
		// repair strands the object in a way that is worse than it sounds --
		// it cannot be deleted either, because removing its finalizer is an
		// update of the whole object and the whole object is invalid. Both
		// shipped components deadlocked exactly there.
		{"class set on an object that has none", component("  class: app\n"), component("  fulfilment: auto\n"), ""},
		{"and still not changed once it has one", component("  class: service\n"), component("  class: app\n"), "class is immutable"},
		{"pointed at another profile", strings.Replace(component("  class: app\n"), "{name: nextcloud}", "{name: odoo}", 1), component("  class: app\n"), "profileRef.name is immutable"},
	}
	for _, c := range cases {
		expect(t, c.name, v.check(t, c.obj, c.old), c.want)
	}
}

// A bridge is a class of its own, reached or reaching out only as its ways
// say. These are the rules the class and its ways carry at admission.
func TestBridgeProfileRules(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")
	head := `
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: x}
spec:
  version: "1.0.0"
  trustTier: certified
`
	class := "  classes: [bridge]\n  launch: none\n"
	bridge := `  bridge:
    app: {profile: nextcloud-base-ce, versions: ["33.0.10"]}
    ways:
    - {name: files, caller: device, callee: app, protocol: webdav, speaksAs: person, paths: [/remote.php/dav/files/]}
    - {name: calendars, caller: device, callee: app, protocol: caldav, speaksAs: person, paths: [/remote.php/dav/calendars/, /remote.php/dav/principals/]}
`
	api := "  package: {api: {backend: {service: nextcloud, port: 8080}}}\n"
	chart := "  package: {chart: {repository: \"oci://charts.example\", name: carddav, version: \"1.0.0\"}}\n"
	ways := func(w string) string {
		return "  bridge:\n    app: {profile: nextcloud-base-ce}\n    ways:\n" + w
	}
	// way is a way from a device to the app in webdav, with these fields
	// put in place of the ones it has.
	way := func(fields string) string {
		base := map[string]string{"name": "files", "caller": "device", "callee": "app", "protocol": "webdav", "speaksAs": "person", "paths": "[/dav/]", "root": "-"}
		for _, kv := range strings.Split(fields, ";") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				base[k] = v
			}
		}
		out := []string{}
		for _, k := range []string{"name", "caller", "callee", "protocol", "speaksAs", "paths", "root"} {
			if v := base[k]; v != "-" {
				out = append(out, k+": "+v)
			}
		}
		return "    - {" + strings.Join(out, ", ") + "}\n"
	}
	cases := []struct{ name, spec, want string }{
		{"a bridge routed to the app's own Service", class + bridge + api, ""},
		{"a bridge that runs a translator", class + bridge + chart, ""},

		{"a bridge that is also an app", "  classes: [bridge, app]\n  launch: none\n" + bridge + api, "bridge is exclusive"},
		{"a bridge without its block", class + api, "spec.bridge is for class bridge"},
		{"an app with a bridge block", "  classes: [app]\n  launch: none\n" + bridge + chart, "spec.bridge is for class bridge"},
		{"a bridge someone opens", "  classes: [bridge]\n  launch: from\n" + bridge + api, "a bridge has launch none"},
		{"a bridge on the gateway", class + bridge + api + "  expose:\n  - {name: web, surface: gateway, authMode: oidc, backend: {service: x, port: 80}}\n", "a bridge exposes, provides and integrates nothing"},
		{"a bridge on the perimeter", class + bridge + api + "  expose:\n  - {name: dav, surface: perimeter, authMode: app, paths: [/dav/], backend: {service: x, port: 80}}\n", "a bridge exposes, provides and integrates nothing"},
		{"a bridge that provides a contract", class + bridge + api + "  provides:\n  - {name: files}\n", "a bridge exposes, provides and integrates nothing"},
		{"a bridge that integrates", class + bridge + api + "  integrations:\n  - {contract: webdav, capabilities: [webdav:read]}\n", "a bridge exposes, provides and integrates nothing"},
		{"a bridge that vouches", "  classes: [bridge]\n  launch: none\n" + bridge + api + "  requires:\n    services:\n      vouching: {}\n", "a bridge does not vouch for people"},
		{"a bridge placed on every tenant", class + bridge + api + "  defaultForTenants: true\n", "never placed by default"},
		{"a bridge that is an addon", class + bridge + "  package: {addon: {id: dav, of: nextcloud-base-ce}}\n", "a bridge is deployed or routed to"},

		{"a bridge pointed outside the cluster", class + bridge + "  package: {api: {baseUrl: \"https://dav.example\"}}\n", "names the Service that answers it"},
		{"a bridge with a tile", class + bridge + "  package: {api: {backend: {service: nextcloud, port: 8080}, tile: {displayName: Files, logo: \"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQU5v2HwADOgHiG7pP+wAAAABJRU5ErkJggg==\", relation: can_launch}}}\n", "carries no tile"},
		{"an api that names both", class + bridge + "  package: {api: {baseUrl: \"https://dav.example\", backend: {service: nextcloud, port: 8080}}}\n", "names where it is answered once"},
		{"an app routed to a Service by an api", "  classes: [app]\n  launch: none\n  package: {api: {backend: {service: nextcloud, port: 8080}}}\n", "package.api.backend is for a bridge"},
		{"an api that names neither", "  classes: [app]\n  launch: none\n  package: {api: {}}\n", "names where it is answered once"},

		{"a bridge in both directions", class + ways(way("")+way("name=tools;caller=agents;callee=app;protocol=mcp;paths=-")) + api, ""},
		{"a service bridge, which connects no app", class + "  bridge:\n    ways:\n" + way("caller=agents;callee=outside;protocol=mcp;paths=-") + api, ""},
		{"a way for the tenant", class + ways(way("name=tools;caller=agents;callee=app;protocol=mcp;speaksAs=tenant;paths=-")) + api, ""},

		{"a bridge with no way", class + ways("") + api, "ways"},
		{"a way to the app that names none", class + "  bridge:\n    ways:\n" + way("") + api, "spec.bridge.app names the app"},
		{"an app no way uses", class + ways(way("caller=agents;callee=outside;protocol=mcp;paths=-")) + api, "spec.bridge.app names the app"},
		{"two ways of one name", class + ways(way("")+way("protocol=caldav")) + api, "a way's name is its own"},
		{"a way to itself", class + ways(way("caller=app;paths=-")) + api, "its caller and its callee differ"},
		{"a device that speaks for the tenant", class + ways(way("speaksAs=tenant")) + api, "speaks as that person"},

		{"a calendar way with a root under its paths", class + ways(way("name=calendars;protocol=caldav;paths=[/dav/calendars/, /dav/principals/];root=/dav/principals/")) + api, ""},
		{"a root that is one of the paths without its slash", class + ways(way("name=contacts;protocol=carddav;paths=[/dav/principals];root=/dav/principals")) + api, ""},
		{"a root outside the way's paths", class + ways(way("name=calendars;protocol=caldav;paths=[/dav/calendars/];root=/dav/")) + api, "under one of its paths"},
		{"a root that only shares a prefix", class + ways(way("name=calendars;protocol=caldav;paths=[/dav/cal];root=/dav/calendars/")) + api, "under one of its paths"},
		{"a root for files", class + ways(way("root=/dav/")) + api, "only for a way in caldav or carddav"},
		{"two calendar ways that name a root", class + ways(way("name=a;protocol=caldav;root=/dav/x")+way("name=b;protocol=caldav;root=/dav/y")) + api, "one discovery address"},
		{"a way from a device with no path", class + ways(way("paths=-")) + api, "paths are where a caller from outside the platform reaches the app"},
		{"paths on a way between agents and the app", class + ways(way("caller=agents;protocol=mcp")) + api, "paths are where a caller from outside the platform reaches the app"},
		{"a caller that is not one", class + ways(way("caller=browser")) + api, "Unsupported value"},
		{"a callee that is not one", class + ways(way("callee=cloud;paths=-")) + api, "Unsupported value"},
		{"a protocol that is not one", class + ways(way("protocol=imap")) + api, "Unsupported value"},
		{"a speaker that is not one", class + ways(way("speaksAs=admin")) + api, "Unsupported value"},
		{"a way with an empty path list", class + ways(way("paths=[]")) + api, "paths"},
		{"the app's root", class + ways(way("paths=[/]")) + api, "paths"},
		{"a path that climbs", class + ways(way("paths=[/dav/../admin/]")) + api, "paths"},
		{"a path with a query", class + ways(way(`paths=["/dav?x=1"]`)) + api, "paths"},
		{"a path that does not begin at the root", class + ways(way("paths=[dav/]")) + api, "paths"},
		{"a way name that is not a label", class + ways(way("name=Files")) + api, "name"},
	}
	for _, c := range cases {
		expect(t, c.name, v.check(t, head+c.spec, ""), c.want)
	}
}

// An adapter is a class of its own, reached by nothing but the component that
// speaks as a person. These are the rules the class carries at admission.
func TestAdapterProfileRules(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")
	adapter := `
  adapter:
    adapts: {profile: openproject, versions: ["15.4.0"]}
`
	head := "  classes: [adapter]\n  launch: none\n  trustTier: certified"
	cases := []struct{ name, spec, want string }{
		{"an adapter", head + adapter, ""},

		{"an adapter that is also an app", "  classes: [adapter, app]\n  launch: none\n  trustTier: certified" + adapter, "adapter is exclusive"},
		{"an adapter without its block", head + "\n", "spec.adapter is for class adapter"},
		{"an app with an adapter block", "  classes: [app]\n  launch: none\n  trustTier: certified" + adapter, "spec.adapter is for class adapter"},
		{"an adapter someone opens", "  classes: [adapter]\n  launch: from\n  trustTier: certified" + adapter, "an adapter has launch none"},
		{"an adapter on the gateway", head + adapter + "  expose:\n  - {name: web, surface: gateway, authMode: oidc, backend: {service: x, port: 80}}\n", "an adapter exposes nothing"},
		{"an adapter that provides a contract", head + adapter + "  provides:\n  - {name: tools}\n", "an adapter provides no contract"},
		{"an adapter that integrates", head + adapter + "  integrations:\n  - {contract: webdav, capabilities: [webdav:read]}\n", "an adapter declares no integrations"},
		{"an adapter that vouches", "  classes: [adapter]\n  launch: none\n  trustTier: platform" + adapter + "  requires:\n    services:\n      vouching: {}\n", "an adapter does not vouch for people"},
		{"an adapter placed on every tenant", head + adapter + "  defaultForTenants: true\n", "never placed by default"},
		{"an adapter that adapts nothing", head + "\n  adapter: {}\n", "spec.adapter.adapts names the app"},
	}
	for _, c := range cases {
		expect(t, c.name, v.check(t, profileHead+c.spec, ""), c.want)
	}

	// An adapter can be an API: a tool server outside the cluster, which it
	// reaches for agents as an API entry does for people. It adapts no app of
	// the tenant, and it is still never an addon.
	apiHead := `
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: x}
spec:
  version: "1.0.0"
  classes: [adapter]
  launch: none
  trustTier: certified
  adapter: {}
`
	for _, c := range []struct{ name, spec, want string }{
		{"an adapter that is an API", "  package: {api: {baseUrl: \"https://tools.example\"}}\n", ""},
		{"an adapter that is an addon", "  package: {addon: {id: tools, of: nextcloud-base-ce}}\n", "an adapter is deployed or reached"},
	} {
		expect(t, c.name, v.check(t, apiHead+c.spec, ""), c.want)
	}

	// An adapter that runs in the tenant and calls a partner as the person
	// adapts no app of the tenant either: a call through it is for the
	// adapter itself. Naming no partner leaves it an adapter of nothing.
	chart := "  package: {chart: {repository: \"oci://charts.example\", name: tools, version: \"1.0.0\"}}\n"
	for _, c := range []struct{ name, spec, want string }{
		{"an adapter that calls a partner", chart + "  requires:\n    services:\n      partners: {call: [store]}\n", ""},
		{"an adapter that calls no partner", chart + "  requires:\n    services:\n      partners: {call: []}\n", "spec.adapter.adapts names the app"},
		{"an adapter that only accepts partners", chart + "  requires:\n    services:\n      partners: {accept: true}\n", "spec.adapter.adapts names the app"},
	} {
		expect(t, c.name, v.check(t, apiHead+c.spec, ""), c.want)
	}

	// A Component of the class: the class is the profile's, and is immutable.
	cv := loadCRD(t, "gentianos.io_components.yaml")
	comp := "apiVersion: gentianos.io/v1alpha1\nkind: Component\nmetadata: {name: x, namespace: tenant-a}\nspec:\n  profileRef: {name: x}\n  class: %s\n"
	expect(t, "an adapter instance", cv.check(t, strings.ReplaceAll(comp, "%s", "adapter"), ""), "")
	expect(t, "an adapter instance made an app", cv.check(t, strings.ReplaceAll(comp, "%s", "app"), strings.ReplaceAll(comp, "%s", "adapter")), "class is immutable")
}

// An action is one pinned image a component runs through the dispatcher.
// These are the rules the class carries at admission.
func TestActionProfileRules(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")
	const image = `{repository: registry.example/steps/ocr, digest: "sha256:` +
		`0000000000000000000000000000000000000000000000000000000000000000"}`
	profile := func(pkg, spec string) string {
		return "\napiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata: {name: x}\nspec:\n" +
			"  version: \"1.0.0\"\n  package: " + pkg + "\n" + spec
	}
	one := "{images: [" + image + "]}"
	action := "  action: {command: [\"/ocr\"]}\n"
	head := "  classes: [action]\n  launch: none\n  trustTier: certified\n"
	chart := `{chart: {repository: "oci://example/charts", name: x, version: "1.0.0"}}`

	for _, c := range []struct{ name, obj, want string }{
		{"an action", profile(one, head+action), ""},
		{"an action with everything said", profile(one, head+
			"  action:\n    command: [\"/ocr\", \"--fast\"]\n    runtime: container\n    deadlineSeconds: 120\n"+
			"    adapters: [nextcloud-files]\n    resources: {cpu: 500m, memory: 256Mi}\n"+
			"    inputs: {type: object, properties: {file: {type: string}}}\n    outputs: {type: object}\n"+
			"  requires:\n    privileges:\n      egress:\n      - name: ocr-service\n        reason: the recognition service\n"+
			"        rule: {ports: [{port: 443}]}\n"), ""},

		{"an action that is also an app", profile(one, "  classes: [action, app]\n  launch: none\n  trustTier: certified\n"+action), "action is exclusive"},
		{"an action without its block", profile(one, head), "spec.action is for class action"},
		{"an app with an action block", profile(chart, "  classes: [app]\n  launch: none\n  trustTier: certified\n"+action), "spec.action is for class action"},
		{"an action someone opens", profile(one, "  classes: [action]\n  launch: from\n  trustTier: certified\n"+action), "an action has launch none"},
		{"an action on the gateway", profile(one, head+action+"  expose:\n  - {name: web, surface: gateway, authMode: oidc, backend: {service: x, port: 80}}\n"), "exposes, provides and integrates nothing"},
		{"an action that provides a contract", profile(one, head+action+"  provides:\n  - {name: tools}\n"), "exposes, provides and integrates nothing"},
		{"an action that integrates", profile(one, head+action+"  integrations:\n  - {contract: webdav, capabilities: [webdav:read]}\n"), "exposes, provides and integrates nothing"},
		{"an action with a chart", profile(chart, head+action), "exactly one pinned image"},
		{"an action with a chart and an image", profile("{chart: {repository: \"oci://example/charts\", name: x, version: \"1.0.0\"}, images: ["+image+"]}", head+action), "exactly one pinned image"},
		{"an action with no image", profile("{}", head+action), "exactly one pinned image"},
		{"an action with two images", profile("{images: ["+image+", {repository: registry.example/steps/other, digest: \"sha256:"+
			"1111111111111111111111111111111111111111111111111111111111111111\"}]}", head+action), "exactly one pinned image"},
		{"an action that is an API", profile("{api: {baseUrl: \"https://steps.example\"}, images: ["+image+"]}", head+action), "exactly one pinned image"},
		{"an action that requires a database", profile(one, head+action+"  requires:\n    services:\n      database: {engine: postgresql}\n"), "an action requires no service"},
		{"an action placed on every tenant", profile(one, head+action+"  defaultForTenants: true\n"), "an action is never placed"},
		{"an action without a command", profile(one, head+"  action: {}\n"), "command"},
		{"an action that runs too long", profile(one, head+"  action: {command: [\"/ocr\"], deadlineSeconds: 90000}\n"), "deadlineSeconds"},
		{"an action on another runtime", profile(one, head+"  action: {command: [\"/ocr\"], runtime: wasm}\n"), "runtime"},
		// A package of images alone is an action's, and nothing else's.
		{"an app with images and nothing to deploy", profile(one, "  classes: [app]\n  launch: none\n  trustTier: certified\n"), "a package is a chart"},
	} {
		expect(t, c.name, v.check(t, c.obj, ""), c.want)
	}
}
