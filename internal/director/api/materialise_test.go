/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
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
// the install asked for (AD-3).
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
	// committed must be what was asked for, and a test that compared some
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
	// Beside it, the same bytes again: the bundle the operator checks a
	// pinned install against at rollout, as a patch the kustomization applies
	// to the profile. Derived from what was served and from nothing else.
	bundle := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/catalogue/"+gitops.BundleFile("element"))
	if !strings.Contains(k, "patches:\n- path: element.bundle.yaml") {
		t.Fatalf("the kustomization does not apply the bundle:\n%s", k)
	}
	var patch struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(bundle), &patch); err != nil {
		t.Fatalf("the bundle is not a document: %v\n%s", err, bundle)
	}
	if patch.APIVersion != "gentianos.io/v1alpha1" || patch.Kind != "ComponentProfile" || patch.Metadata.Name != "element" {
		t.Fatalf("the bundle patches something other than the profile: %+v", patch)
	}
	carried, err := base64.StdEncoding.DecodeString(patch.Metadata.Annotations[profilebundle.Annotation])
	if err != nil || string(carried) != elementProfile {
		t.Fatalf("the bundle does not carry the bytes that were served: %v\n%s", err, carried)
	}
	// And the two halves meet: the profile as the cluster then holds it --
	// the document with the patch's annotation on it -- is what the operator
	// accepts for an install pinned to this digest, and for no other.
	live := &gentianov1alpha1.ComponentProfile{}
	if err := yaml.Unmarshal([]byte(got), live); err != nil {
		t.Fatal(err)
	}
	live.Annotations = patch.Metadata.Annotations
	if r := profilebundle.Verify(live, sha(elementProfile)); r != nil {
		t.Fatalf("the operator would hold what the director just installed: %s", r.Message)
	}
	if r := profilebundle.Verify(live, sha("another build")); r == nil {
		t.Fatal("the committed profile verifies against a digest it is not")
	}
	// The commit says which digest was checked, so a reviewer need not hash
	// the file to know whether it is the build that was asked for.
	log := dt.Git(t, "", "--git-dir", h.remote, "log", "--format=%s", "-3", "main")
	if !strings.Contains(log, "materialise element at sha256:") {
		t.Fatalf("no materialise commit:\n%s", log)
	}
	// And the tenant's entry is pinned to the same build, as a field.
	tenant := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(tenant, "  - profile: element\n    digest: "+sha(elementProfile)+"\n") {
		t.Fatalf("the install does not record its digest:\n%s", tenant)
	}
	// And it says which catalogue the build was fetched from, which with the
	// name is the coordinate.
	if !strings.Contains(tenant, "    catalogue: main\n") {
		t.Fatalf("the install does not record its catalogue:\n%s", tenant)
	}
}

// A second tenant installing the same entry at the same digest finds the
// profile and its bundle already there, and commits neither again.
func TestInstallingWhatIsAlreadyMaterialisedCommitsItOnce(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	body := fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", h.token(t, "tenant-demo", "tom"), body); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/solo/apps/element", h.token(t, "tenant-solo", "tina"), body); code != http.StatusAccepted {
		t.Fatalf("second install = %d %v", code, out)
	}
	log := dt.Git(t, "", "--git-dir", h.remote, "log", "--format=%s", "main")
	if n := strings.Count(log, "materialise element at sha256:"); n != 1 {
		t.Fatalf("the entry was materialised %d times:\n%s", n, log)
	}
}

// An entry too large for its bytes to travel with it is refused outright. It
// could be committed, and the install would then be pinned to a digest the
// operator can never check, which holds it at rollout for good.
func TestAnEntryTooLargeToCarryItsBundleInstallsNothing(t *testing.T) {
	huge := elementProfile + "# " + strings.Repeat("x", profilebundle.MaxBytes) + "\n"
	src := catalogueSource(t, huge)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	before := h.tip(t)
	body := fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(huge))
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", h.token(t, "tenant-demo", "tom"), body)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("install = %d %v, want 422", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("it committed anyway")
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

	// The digest the REQUEST named, against bytes the SOURCE changed.
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
	// And a catalogue the caller only named is not written down as where the
	// app came from.
	if tenant := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(tenant, "catalogue:") {
		t.Fatalf("a catalogue nothing was fetched from was recorded:\n%s", tenant)
	}
}

// A coordinate that names another app than the one being installed is refused:
// it would commit one profile and install another.
func TestTheCoordinateMustNameTheAppBeingInstalled(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	body := fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, body); code != http.StatusBadRequest {
		t.Fatalf("install = %d %v, want 400", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("it committed anyway")
	}
}

// An install from a source is asked of the person and of nothing else: there
// is no question about the source, which the Cluster claim offers to every
// tenant.
func TestInstallingFromASourceAsksOnlyWhetherThePersonMayInstall(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	body := fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))

	tina := h.token(t, "tenant-solo", "tina")
	h.asked.reset()
	if code, out := h.do(t, "POST", "/v1/tenants/solo/apps/element", tina, body); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if got := h.asked.questions(); len(got) != 1 || got[0] != "user:tina can_install_app tenant:solo" {
		t.Fatalf("the install asked %q", got)
	}
}
