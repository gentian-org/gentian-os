/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// An addon is pinned by digest like an app: fetched from a declared source,
// checked, committed with its bundle, and recorded on the entry.

func addonProfile(name string) string {
	return `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: ` + name + `
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    addon:
      of: element
`
}

// addonSource serves profiles by name, and can be told to serve something
// else under one -- which is what a compromised source is.
func addonSource(t *testing.T, profiles map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/profiles/"), ".yaml")
		body, ok := profiles[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pinnedAddon(coordinate, digest string) string {
	return fmt.Sprintf(`{"coordinate":%q,"digest":%q}`, coordinate, digest)
}

func tenantApps(t *testing.T, h *harness) []gitops.App {
	t.Helper()
	var doc struct {
		Spec struct {
			Apps []gitops.App `json:"apps"`
		} `json:"spec"`
	}
	file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil {
		t.Fatalf("the manifest does not parse: %v\n%s", err, file)
	}
	return doc.Spec.Apps
}

func TestAnAddonIsInstalledAtACoordinateAndDigest(t *testing.T) {
	talk, deck := addonProfile("element-talk"), addonProfile("element-deck")
	src := addonSource(t, map[string]string{"element": elementProfile, "element-talk": talk, "element-deck": deck})
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}

	// One pinned, one by name: the name behaves as it always has. The digest
	// is stated in capitals and recorded in the one spelling the schema admits.
	h.asked.reset()
	code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":["calendar",`+pinnedAddon("main/element-talk", strings.ToUpper(sha(talk)))+`]}`)
	if code != http.StatusAccepted || out["status"] != "updated" {
		t.Fatalf("set addons = %d %v", code, out)
	}
	// Asked of the person and of nothing else, like an install.
	if got := h.asked.questions(); len(got) != 1 || got[0] != "user:tom can_install_app tenant:demo" {
		t.Fatalf("setting addons asked %q", got)
	}

	// The addon's profile is in the repository as served, with its bundle.
	dir := "clusters/" + dt.Cluster + "/catalogue/"
	if got := dt.RemoteFile(t, h.remote, dir+"element-talk.yaml"); sha(strings.TrimRight(got, "\n")+"\n") != sha(talk) {
		t.Fatalf("what was committed is not what was served:\n%s", got)
	}
	var patch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(dt.RemoteFile(t, h.remote, dir+gitops.BundleFile("element-talk"))), &patch); err != nil {
		t.Fatal(err)
	}
	carried, err := base64.StdEncoding.DecodeString(patch.Metadata.Annotations[profilebundle.Annotation])
	if err != nil || profilebundle.Digest(carried) != sha(talk) {
		t.Fatalf("the bundle beside the addon is not the build that was pinned (%v)", err)
	}
	k := dt.RemoteFile(t, h.remote, dir+"kustomization.yaml")
	if !strings.Contains(k, "element-talk.yaml") || !strings.Contains(k, "element-talk.bundle.yaml") {
		t.Fatalf("kustomization:\n%s", k)
	}

	// The pin is on the entry, beside the list of names.
	apps := tenantApps(t, h)
	if apps[0].Profile != "element" || strings.Join(apps[0].Addons, ",") != "calendar,element-talk" || len(apps[0].AddonPins) != 1 ||
		apps[0].AddonPins[0] != (gitops.AddonPin{Name: "element-talk", Digest: sha(talk), Catalogue: "main"}) {
		t.Fatalf("apps = %+v", apps)
	}
	if apps[0].Digest != sha(elementProfile) {
		t.Fatalf("the app's own pin was disturbed: %+v", apps[0])
	}
	if subject := dt.Git(t, "", "--git-dir", h.remote, "log", "--format=%s", "-1", "main"); !strings.Contains(subject, "pinning element-talk at "+sha(talk)[:19]) {
		t.Fatalf("the commit does not say which build: %q", subject)
	}

	// Both reads return it.
	mia := h.token(t, "tenant-demo", "mia")
	code, out = h.do(t, "GET", "/v1/tenants/demo/apps", mia, "")
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out["apps"]), "addonPins:[map[catalogue:main digest:"+sha(talk)+" name:element-talk]]") {
		t.Fatalf("apps = %d %v", code, out)
	}
	code, out = h.do(t, "GET", "/v1/tenants/demo/apps/element/addons", mia, "")
	if code != http.StatusOK || fmt.Sprint(out["addons"]) != "[calendar element-talk]" ||
		!strings.Contains(fmt.Sprint(out["addonPins"]), "digest:"+sha(talk)) {
		t.Fatalf("addons = %d %v", code, out)
	}

	// The same selection again changes nothing.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":["calendar",`+pinnedAddon("main/element-talk", sha(talk))+`]}`); code != http.StatusOK || out["status"] != "no_change" {
		t.Fatalf("the same selection again = %d %v", code, out)
	}

	// Names only, as a caller that knows nothing of pins sends them: an
	// addon that stays keeps its pin, and one that is added has none.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":["element-talk","element-deck"]}`); code != http.StatusAccepted {
		t.Fatalf("names only = %d %v", code, out)
	}
	apps = tenantApps(t, h)
	if strings.Join(apps[0].Addons, ",") != "element-talk,element-deck" || len(apps[0].AddonPins) != 1 || apps[0].AddonPins[0].Name != "element-talk" {
		t.Fatalf("after names only, apps = %+v", apps)
	}

	// An addon that leaves the list takes its pin with it.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom, `{"addons":["element-deck"]}`); code != http.StatusAccepted {
		t.Fatalf("deselecting = %d %v", code, out)
	}
	if file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(file, "addonPins") || strings.Contains(file, sha(talk)) {
		t.Fatalf("the pin of a deselected addon was left behind:\n%s", file)
	}
}

// What an addon's pin is refused for, each before anything is written -- and
// with it the whole selection, so no part of a refused request takes effect.
func TestAnAddonWhoseBuildCannotBeVerifiedIsRefused(t *testing.T) {
	talk, deck := addonProfile("element-talk"), addonProfile("element-deck")
	served := map[string]string{"element": elementProfile, "element-talk": talk, "element-deck": "something else\n"}
	src := addonSource(t, served)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	// The base is installed at a stated build, so that what is refused below
	// is refused for the addon's own build and not for the base's.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	before := h.tip(t)

	good := pinnedAddon("main/element-talk", sha(talk))
	for name, c := range map[string]struct {
		entry string
		code  int
		says  string
	}{
		"a catalogue the cluster does not declare": {pinnedAddon("elsewhere/element-talk", sha(talk)), http.StatusUnprocessableEntity, "catalogue sources: main."},
		"a digest with no coordinate":              {fmt.Sprintf(`{"digest":%q}`, sha(talk)), http.StatusUnprocessableEntity, "coordinate"},
		"a coordinate with no digest":              {`{"coordinate":"main/element-talk"}`, http.StatusBadRequest, "digest"},
		"a digest that is not one":                 {pinnedAddon("main/element-talk", "latest"), http.StatusBadRequest, "sha256"},
		"a coordinate that is not one":             {pinnedAddon("element-talk", sha(talk)), http.StatusBadRequest, "<catalogue>/<app>"},
		"neither":                                  {`{}`, http.StatusBadRequest, "an addon is a name"},
		"a source serving another build":           {pinnedAddon("main/element-deck", sha(deck)), http.StatusBadGateway, "not this entry"},
		"an entry the source does not serve":       {pinnedAddon("main/element-poll", sha(talk)), http.StatusNotFound, "does not serve"},
		"the same addon twice":                     {`"element-talk"`, http.StatusBadRequest, "twice"},
	} {
		// After a pin that would verify: it is not committed either.
		code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom, `{"addons":[`+good+`,`+c.entry+`]}`)
		if code != c.code || !strings.Contains(fmt.Sprint(out["error"]), c.says) {
			t.Fatalf("%s: %d %v, want %d saying %q", name, code, out, c.code, c.says)
		}
		if h.tip(t) != before {
			t.Fatalf("%s: a refused selection moved the repository", name)
		}
	}

	// An unknown field is not quietly dropped: a store's item sent whole,
	// version and all, is a request this route does not understand.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":[{"coordinate":"main/element-talk","digest":"`+sha(talk)+`","version":"1.0.0"}]}`); code != http.StatusBadRequest {
		t.Fatalf("an entry with an unknown field = %d, want 400", code)
	}

	// On a cluster that declares no source, a pinned addon is refused and a
	// name is set as before.
	bare := start(t)
	tom = bare.token(t, "tenant-demo", "tom")
	if code, out := bare.do(t, "POST", "/v1/tenants/demo/apps/element", tom, ""); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if code, out := bare.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom, `{"addons":[`+good+`]}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(fmt.Sprint(out["error"]), "declares no catalogue source") {
		t.Fatalf("a pinned addon with no source declared = %d %v", code, out)
	}
	if code, out := bare.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom, `{"addons":["element-talk"]}`); code != http.StatusAccepted {
		t.Fatalf("a name with no source declared = %d %v", code, out)
	}
}

// A pinned addon for an app the tenant does not have fetches and commits
// nothing.
func TestAPinnedAddonOfAnAppThatIsNotInstalledCommitsNothing(t *testing.T) {
	talk := addonProfile("element-talk")
	src := addonSource(t, map[string]string{"element-talk": talk})
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":[`+pinnedAddon("main/element-talk", sha(talk))+`]}`)
	if code != http.StatusOK || out["status"] != "not_installed" {
		t.Fatalf("set addons = %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a profile was materialised for an app that is not installed")
	}
}

// An addon is pinned only inside a pinned app: a stated build of an addon in
// an app installed at none is refused before anything is fetched, and says
// how the app is pinned. Names inside such an app are set as they always
// were, and the same request is accepted once the app carries a digest.
func TestAPinnedAddonIsRefusedInsideAnAppThatIsNotPinned(t *testing.T) {
	talk := addonProfile("element-talk")
	fetched := 0
	inner := addonSource(t, map[string]string{"element": elementProfile, "element-talk": talk})
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched++
		resp, err := inner.Client().Get(inner.URL + r.URL.Path)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(src.Close)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, ""); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	before, fetchedBefore := h.tip(t), fetched

	pin := pinnedAddon("main/element-talk", sha(talk))
	code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom, `{"addons":["calendar",`+pin+`]}`)
	said := fmt.Sprint(out["error"])
	if code != http.StatusUnprocessableEntity ||
		!strings.Contains(said, "kubectl gentian apps install element --tenant demo") ||
		!strings.Contains(said, "stated build") {
		t.Fatalf("a pinned addon inside an unpinned app = %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused pin moved the repository")
	}
	if fetched != fetchedBefore {
		t.Fatalf("a refused pin fetched from the source %d time(s)", fetched-fetchedBefore)
	}

	// Names, with no build stated, inside the same unpinned app.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":["calendar","element-talk"]}`); code != http.StatusAccepted {
		t.Fatalf("names inside an unpinned app = %d %v", code, out)
	}
	if apps := tenantApps(t, h); strings.Join(apps[0].Addons, ",") != "calendar,element-talk" || len(apps[0].AddonPins) != 0 {
		t.Fatalf("apps = %+v", apps)
	}

	// Once the app is at a stated build, the request that was refused is
	// accepted.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))); code != http.StatusAccepted {
		t.Fatalf("pinning the app = %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":["calendar",`+pin+`]}`); code != http.StatusAccepted {
		t.Fatalf("the same pin inside a pinned app = %d %v", code, out)
	}
	if apps := tenantApps(t, h); len(apps[0].AddonPins) != 1 || apps[0].AddonPins[0].Digest != sha(talk) {
		t.Fatalf("apps = %+v", apps)
	}
}
