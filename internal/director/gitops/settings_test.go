/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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

// The first list-valued setting. Inserted under its parent when the claim
// lacks it, as a block list; replaced whole when it is there; a commented
// placeholder is its home like a scalar's; the same list again is no commit;
// an empty list is written as one. Read back joined, the shape the API and
// the installer carry it in.
func TestAListSettingIsWrittenAsABlockAndReadBackJoined(t *testing.T) {
	t.Parallel()
	out, changed, err := setClaimList(claimFixture, "certificates.dnsZones", []string{"gentian.org", "example.ch"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(out, "    dnsProvider: cloudflare\n    dnsZones:\n      - gentian.org\n      - example.ch\n  mail:") {
		t.Fatalf("list not written under certificates:\n%s", out)
	}
	var claim map[string]any
	if err := yaml.Unmarshal([]byte(out), &claim); err != nil {
		t.Fatalf("result is not YAML: %v\n%s", err, out)
	}
	spec := claim["spec"].(map[string]any)
	if got, _ := lookupPath(spec, []string{"certificates", "dnsZones"}); got != "gentian.org,example.ch" {
		t.Fatalf("read back %q", got)
	}

	// The same list again changes nothing, whatever the spacing it came in.
	again, changed, err := setClaimList(out, "certificates.dnsZones", splitItems("gentian.org, example.ch"))
	if err != nil || changed || again != out {
		t.Fatalf("the same list must not be a commit: changed=%v err=%v", changed, err)
	}

	// A different list replaces the whole block, and nothing after it moves.
	shorter, changed, err := setClaimList(out, "certificates.dnsZones", []string{"example.ch"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if strings.Contains(shorter, "gentian.org") || !strings.Contains(shorter, "    dnsZones:\n      - example.ch\n  mail:\n    serviceMode: system") {
		t.Fatalf("list not replaced:\n%s", shorter)
	}

	// Emptied, it is an empty list, not an absent key: set to nothing is a
	// real answer.
	none, changed, err := setClaimList(shorter, "certificates.dnsZones", nil)
	if err != nil || !changed || !strings.Contains(none, "    dnsZones: []\n  mail:") {
		t.Fatalf("empty list: changed=%v err=%v\n%s", changed, err, none)
	}
	// And a flow list is read as one, so filling it again is a change from [].
	filled, changed, err := setClaimList(none, "certificates.dnsZones", []string{"gentian.org"})
	if err != nil || !changed || !strings.Contains(filled, "    dnsZones:\n      - gentian.org\n") {
		t.Fatalf("from []: changed=%v err=%v\n%s", changed, err, filled)
	}
	if _, changed, _ = setClaimList(none, "certificates.dnsZones", nil); changed {
		t.Fatal("[] set to [] must not be a commit")
	}
}

// The claim scaffold documents dnsZones as a commented key. Choosing it uses
// that line, as for a scalar, and the comments around it survive.
func TestACommentedListPlaceholderBecomesTheList(t *testing.T) {
	t.Parallel()
	fixture := strings.Replace(claimFixture, "    dnsProvider: cloudflare\n",
		"    dnsProvider: cloudflare\n    # dnsZones:                 zones of tenants served on a domain of\n    #                           their own\n", 1)
	out, changed, err := setClaimList(fixture, "certificates.dnsZones", []string{"gentian.org"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(out, "    dnsProvider: cloudflare\n    dnsZones:\n      - gentian.org\n    #                           their own\n") {
		t.Fatalf("placeholder not used:\n%s", out)
	}
	if strings.Count(out, "dnsZones") != 1 {
		t.Fatal("the list was added beside its placeholder instead of in it")
	}
	var probe map[string]any
	if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("result is not YAML: %v\n%s", err, out)
	}
}

// What a list setting takes: zone names, each checked, and nothing else.
func TestAListSettingChecksEveryItem(t *testing.T) {
	t.Parallel()
	s, ok := settingByPath("certificates.dnsZones")
	if !ok || !s.List || s.InstallStep != "B-01" {
		t.Fatalf("certificates.dnsZones should be a list the installer reads at B-01: %+v", s)
	}
	for _, good := range []string{"gentian.org", "gentian.org,example.ch", " gentian.org , example.ch ", ""} {
		if err := validateSetting(s, good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
	for _, bad := range []string{"gentian", "www.gentian.org/", "Gentian.org", "a b.org"} {
		if err := validateSetting(s, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
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
		"off":                    `"off"`,
		"On":                     `"On"`,
		"no":                     `"no"`,
		"y":                      `"y"`,
		"offline":                "offline",
	} {
		if got := quoteScalar(in); got != want {
			t.Errorf("quoteScalar(%q) = %s, want %s", in, got, want)
		}
	}
}

// A setting the console offers must be a path the claim can carry, and the
// default it shows must be the one the schema applies. Both come from the
// Cluster XRD; this asserts the catalog agrees with it, so a setting that
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
			t.Errorf("%s is not in the catalog", path)
			continue
		}
		if got.Default != want {
			t.Errorf("%s default = %q, the XRD says %q", path, got.Default, want)
		}
	}
	// A setting the schema gives no default keeps none: unset is a real
	// answer and must not be dressed up as a value.
	if d := byPath["mail.host"].Default; d != "" {
		t.Errorf("mail.host has no default in the schema, catalog says %q", d)
	}
}
