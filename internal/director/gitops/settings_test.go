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

package gitops

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// A claim shaped like the one a cluster is installed from: comments that
// explain each setting, a commented placeholder for one nobody chose, and a
// nested block.
const claimFixture = `apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: gentian-os-dev
spec:
  layout: v5
  kernelDomain: gentian-os.org
  # Who administers this cluster, by the Keycloak group they are in.
  platformRoles:
    admin: gentian:platform:admin

  # How traffic reaches this cluster.
  networkMode: tunnel
  # nodeIp:                      not used while networkMode is tunnel

  # Who issues TLS certificates.
  certificates:
    issuerMode: acme-dns01
    # acmeEnv:                   staging until the cluster is proven
    dnsProvider: cloudflare
  mail:
    serviceMode: system
`

func TestASettingIsReplacedInPlaceAndTheCommentsSurvive(t *testing.T) {
	t.Parallel()
	out, changed, err := setClaimValue(claimFixture, "mail.serviceMode", "external")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(out, "    serviceMode: external") {
		t.Fatalf("value not written:\n%s", out)
	}
	for _, comment := range []string{
		"# Who administers this cluster",
		"# How traffic reaches this cluster.",
		"# Who issues TLS certificates.",
	} {
		if !strings.Contains(out, comment) {
			t.Errorf("comment lost: %s", comment)
		}
	}
	// Nothing else moved.
	if strings.Count(out, "\n") != strings.Count(claimFixture, "\n") {
		t.Errorf("the file changed length; a line editor must not")
	}
	var probe map[string]any
	if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("result is not YAML: %v", err)
	}
}

// The claim documents a setting nobody has chosen as a commented key. Choosing
// it should use that line rather than adding a second one somewhere else.
func TestACommentedPlaceholderBecomesTheSetting(t *testing.T) {
	t.Parallel()
	out, changed, err := setClaimValue(claimFixture, "certificates.acmeEnv", "production")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(out, "    acmeEnv: production") {
		t.Fatalf("placeholder not used:\n%s", out)
	}
	if strings.Count(out, "acmeEnv") != 1 {
		t.Errorf("acmeEnv written twice:\n%s", out)
	}
	var claim struct {
		Spec struct {
			Certificates struct {
				AcmeEnv    string `json:"acmeEnv"`
				IssuerMode string `json:"issuerMode"`
			} `json:"certificates"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &claim); err != nil {
		t.Fatalf("result is not YAML: %v", err)
	}
	if claim.Spec.Certificates.AcmeEnv != "production" || claim.Spec.Certificates.IssuerMode != "acme-dns01" {
		t.Fatalf("parsed = %+v", claim.Spec.Certificates)
	}
}

// A setting whose block does not exist yet gets the block.
func TestAMissingSettingIsInsertedUnderItsParent(t *testing.T) {
	t.Parallel()
	out, changed, err := setClaimValue(claimFixture, "llm.enabled", "true")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var claim struct {
		Spec struct {
			LLM struct {
				Enabled bool `json:"enabled"`
			} `json:"llm"`
			KernelDomain string `json:"kernelDomain"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &claim); err != nil {
		t.Fatalf("result is not YAML: %v\n%s", err, out)
	}
	if !claim.Spec.LLM.Enabled {
		t.Fatalf("llm.enabled not set:\n%s", out)
	}
	if claim.Spec.KernelDomain != "gentian-os.org" {
		t.Fatal("an unrelated setting changed")
	}
}

// Setting a value it already has is not a commit.
func TestSettingWhatIsAlreadyThereChangesNothing(t *testing.T) {
	t.Parallel()
	out, changed, err := setClaimValue(claimFixture, "mail.serviceMode", "system")
	if err != nil {
		t.Fatal(err)
	}
	if changed || out != claimFixture {
		t.Fatal("an unchanged value must not produce a commit")
	}
}

// The allowlist is the boundary: a path outside it is refused, and so is a
// value outside what a setting accepts.
func TestOnlyKnownSettingsAndKnownValues(t *testing.T) {
	t.Parallel()
	if _, ok := settingByPath("masterPasswordSecretRef.name"); ok {
		t.Fatal("a secret reference must not be settable")
	}
	if _, ok := settingByPath("openbao.server"); ok {
		t.Fatal("the vault's address must not be settable")
	}
	s, ok := settingByPath("mail.serviceMode")
	if !ok || len(s.OneOf) == 0 {
		t.Fatal("mail.serviceMode should be constrained")
	}
}

// Values that YAML would read as something other than a string are quoted,
// and ordinary ones are left as a person would write them.
func TestScalarsAreQuotedOnlyWhenTheyMustBe(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"external":               "external",
		"true":                   "true",
		"587":                    "587",
		"gentian:platform:admin": `"gentian:platform:admin"`,
		"":                       `""`,
	} {
		if got := quoteScalar(in); got != want {
			t.Errorf("quoteScalar(%q) = %s, want %s", in, got, want)
		}
	}
}

// A setting the console offers must be a path the claim can carry, and the
// default it shows must be the one the schema applies. Both come from the
// Cluster XRD; this asserts the catalogue agrees with it, so a setting that
// would commit cleanly and change nothing fails here rather than in a
// cluster.
func TestEverySettingsDefaultComesFromTheSchema(t *testing.T) {
	byPath := map[string]ClusterSetting{}
	for _, s := range ClusterSettings() {
		byPath[s.Path] = s
	}
	for path, want := range map[string]string{
		"certificates.acmeEnv":                 "production",
		"certificates.issuerMode":              "acme-dns01",
		"mail.serviceMode":                     "external",
		"mail.port":                            "587",
		"mail.starttls":                        "true",
		"mail.ssl":                             "false",
		"llm.enabled":                          "false",
		"tenancyMode":                          "multi",
		"platformRoles.admin":                  "gentian:platform:admin",
		"tenantDefaults.limitRange.defaultCpu": "500m",
		"tenantDefaults.limitRange.defaultRequestCpu": "100m",
	} {
		got, ok := byPath[path]
		if !ok {
			t.Errorf("%s is not in the catalogue", path)
			continue
		}
		if got.Default != want {
			t.Errorf("%s default = %q, the XRD says %q", path, got.Default, want)
		}
	}
	// A setting the schema gives no default keeps none: unset is a real
	// answer and must not be dressed up as a value.
	if d := byPath["mail.host"].Default; d != "" {
		t.Errorf("mail.host has no default in the schema, catalogue says %q", d)
	}
}
