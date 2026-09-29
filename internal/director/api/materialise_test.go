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

package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

const elementProfile = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: element
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/element
      name: element
      version: "1.0.0"
`

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// catalogueSource serves a profile bundle, and can be told to serve something
// else at the same URL — which is what a compromised source is.
func catalogueSource(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/profiles/element.yaml") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A profile arrives when a tenant installs it, and is committed at the digest
// the store named (AD-3).
func TestInstallingMaterialisesTheProfile(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")

	body := fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, body)
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}

	// The profile is in the deployments repository, byte for byte as served.
	//
	// Compared by DIGEST rather than by string, and trimmed, because the
	// helper reads through `git show`, which does not promise the trailing
	// byte back. The digest is the thing that has to hold anyway: what was
	// committed must be what the store named, and a test that compared some
	// other way would be checking something weaker than the code does.
	path := "clusters/" + dt.Cluster + "/catalogue/element.yaml"
	got := dt.RemoteFile(t, h.remote, path)
	if sha(strings.TrimRight(got, "\n")+"\n") != sha(elementProfile) {
		t.Fatalf("what was committed is not what was served:\n%s", got)
	}
	// And the kustomization lists it, or Argo syncs a file nothing references.
	k := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/catalogue/kustomization.yaml")
	if !strings.Contains(k, "element.yaml") {
		t.Fatalf("kustomization:\n%s", k)
	}
	// The commit says which digest was checked, so a reviewer need not hash
	// the file to know whether it is the entry the store meant.
	log := dt.Git(t, "", "--git-dir", h.remote, "log", "--format=%s", "-3", "main")
	if !strings.Contains(log, "materialise element at sha256:") {
		t.Fatalf("no materialise commit:\n%s", log)
	}
}

// The property the design rests on: the source is not trusted. A source
// serving something else fails the install and writes nothing — neither the
// profile nor the tenant's app entry.
func TestATamperedBundleInstallsNothing(t *testing.T) {
	tampered := strings.Replace(elementProfile,
		"oci://example.invalid/element", "oci://attacker.invalid/element", 1)
	src := catalogueSource(t, tampered)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	// The digest the STORE named, against bytes the SOURCE changed.
	body := fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, body)
	if code != http.StatusBadGateway {
		t.Fatalf("install = %d %v, want 502", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install committed something")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "nothing was installed") {
		t.Fatalf("the refusal does not say nothing happened: %q", msg)
	}
}

// Installing from a source without the digest is refused: there would be
// nothing to check the bytes against.
func TestMaterialisingNeedsTheDigest(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, `{"coordinate":"main/element"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("install with no digest = %d, want 400", code)
	}
	if h.tip(t) != before {
		t.Fatal("it committed anyway")
	}
}

// A catalogue this cluster has no source for is left alone: a cluster may
// name a source for one and sync another wholesale.
func TestAnUnknownCatalogueInstallsAsBefore(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")

	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		`{"coordinate":"elsewhere/element"}`)
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	// Nothing was materialised for a catalogue with no source.
	log := dt.Git(t, "", "--git-dir", h.remote, "log", "--format=%s", "-5", "main")
	if strings.Contains(log, "materialise") {
		t.Fatalf("it materialised from a source it does not have:\n%s", log)
	}
}

// AD-14: a catalogue the Cluster claim opened to a tenant installs without a
// statement from the store, and only for the tenants the claim names.
//
// Needs the model to decide, because what is being tested IS the model: the
// entry's source is open to this tenant, therefore the entry is installable.
// A table would only restate the answer.
func TestATenantInstallsFromACatalogueTheClaimOpenedToIt(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithEntitledCatalogue(t, true, src, map[string]string{"in-house": src.URL})
	if h.graph == nil {
		t.Skip("needs OpenFGA deciding: make test-director-contract")
	}
	ctx := context.Background()
	if err := h.graph.ReconcileCatalogueSources(ctx, []string{"demo", "solo"},
		map[string][]string{"in-house": {"demo"}}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"coordinate":"in-house/element","digest":%q}`, sha(elementProfile))
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, body); code != http.StatusAccepted {
		t.Fatalf("install from an open catalogue = %d %v", code, out)
	}

	// And a tenant the claim did not name gets nothing, from the same source.
	tina := h.token(t, "tenant-solo", "tina")
	before := h.tip(t)
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/element", tina, body); code != http.StatusForbidden {
		t.Fatalf("a tenant the claim did not name installed anyway: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install committed something")
	}
}

// The other half: an entry of an ENTITLED source still needs the store to
// have said so, and recording which catalogue serves it grants nothing.
func TestBindingAnEntryToItsSourceGrantsNothingByItself(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithEntitledCatalogue(t, true, src, map[string]string{"in-house": src.URL})
	if h.graph == nil {
		t.Skip("needs OpenFGA deciding: make test-director-contract")
	}
	if err := h.graph.ReconcileCatalogueSources(context.Background(),
		[]string{"demo", "solo"}, nil); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"coordinate":"in-house/element","digest":%q}`, sha(elementProfile))
	tom := h.token(t, "tenant-demo", "tom")
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, body); code != http.StatusForbidden {
		t.Fatalf("an entry of a source nothing opened installed: %d", code)
	}
}
