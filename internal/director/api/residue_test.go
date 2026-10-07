/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// residueOperator stands in for the operator's two residue routes: the list
// it would answer, and what it says to each removal it is asked for.
type residueOperator struct {
	mu sync.Mutex
	// unused are the profiles the list names as unused.
	unused []string
	// answers are given to the removals in order; the last one repeats.
	answers []residueAnswer
	// asked is every removal that arrived, as "<actor>: <body>".
	asked []string
	reads int
}

type residueAnswer struct {
	status int
	body   string
}

func deletedAnswer(kind, name string) residueAnswer {
	return residueAnswer{http.StatusOK, fmt.Sprintf(
		`{"status":"deleted","deleted":{"kind":%q,"name":%q,"class":"dropped"},"message":"the %s %s was deleted"}`,
		kind, name, kind, name)}
}

func refusedAnswer(reason, detail string) residueAnswer {
	return residueAnswer{http.StatusConflict, fmt.Sprintf(`{"detail":%q,"reason":%q}`, detail, reason)}
}

func (o *residueOperator) Get(_ context.Context, path string, _ url.Values) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if path != "/v1/catalogue/residue" {
		return http.StatusNotFound, []byte(`{"detail":"no such read"}`), nil
	}
	o.reads++
	items := []map[string]any{}
	for _, name := range o.unused {
		items = append(items, map[string]any{"kind": "ComponentProfile", "name": name, "class": "unused-profile"})
	}
	body, _ := json.Marshal(map[string]any{"residue": items, "incomplete": []string{}})
	return http.StatusOK, body, nil
}

func (o *residueOperator) Do(_ context.Context, path, actor string, body any) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if path != "/v1/actions/remove-catalogue-residue" {
		return http.StatusNotFound, []byte(`{"detail":"no such action"}`), nil
	}
	raw, _ := json.Marshal(body)
	o.asked = append(o.asked, actor+": "+string(raw))
	if len(o.answers) == 0 {
		return http.StatusInternalServerError, []byte(`{"detail":"no answer was arranged"}`), nil
	}
	answer := o.answers[0]
	if len(o.answers) > 1 {
		o.answers = o.answers[1:]
	}
	return answer.status, []byte(answer.body), nil
}

func (o *residueOperator) removals() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.asked...)
}

func (o *residueOperator) Plans(context.Context, string, bool) ([]lifecycle.Plan, error) {
	return nil, nil
}

func (o *residueOperator) Stream(context.Context, string) (*http.Response, error) {
	return nil, errors.New("no streams here")
}

func (o *residueOperator) Upload(context.Context, string, string, io.Reader) (int, []byte, error) {
	return http.StatusNotFound, nil, nil
}

const removeResidue = "/v1/clusters/" + dt.Cluster + "/actions/remove-catalogue-residue"

// Removing residue is the cluster administrator's, asks can_configure of
// the cluster and nothing else, and deletes nothing until the name has been
// typed again.
func TestRemovingResidueIsGuardedAndAsksForTheNameAgain(t *testing.T) {
	op := &residueOperator{answers: []residueAnswer{deletedAnswer("Composition", "app-odoo")}}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)

	// Whoever may not configure the cluster: an auditor of it, a tenant's
	// administrator, a member.
	for who, token := range map[string]string{
		"an auditor":               h.token(t, "gentian", "audrey"),
		"a tenant's administrator": h.token(t, "tenant-demo", "tom"),
		"a member":                 h.token(t, "tenant-demo", "mia"),
	} {
		if code, _ := h.do(t, "POST", removeResidue, token, `{"kind":"Composition","name":"app-odoo","confirm":"app-odoo"}`); code != http.StatusForbidden {
			t.Errorf("%s removed residue: %d", who, code)
		}
	}
	if code, _ := h.do(t, "POST", removeResidue, "", `{"kind":"Composition","name":"app-odoo","confirm":"app-odoo"}`); code != http.StatusUnauthorized {
		t.Errorf("nobody removed residue: %d", code)
	}

	// Without the name again, and with another name: 428, in the shape the
	// director's other dangerous changes answer in.
	for _, body := range []string{
		`{"kind":"Composition","name":"app-odoo"}`,
		`{"kind":"Composition","name":"app-odoo","confirm":"app-default"}`,
		`{"kind":"Composition","name":"app-odoo","confirm":"yes"}`,
	} {
		h.asked.reset()
		code, out := h.do(t, "POST", removeResidue, alice, body)
		if code != http.StatusPreconditionRequired || out["confirmField"] != "confirm" || out["confirmWith"] != "app-odoo" ||
			out["dangerous"] != true || out["requiresRetype"] != true {
			t.Fatalf("unconfirmed %s: %d %v", body, code, out)
		}
		if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:alice can_configure cluster:"+dt.Cluster {
			t.Errorf("asked %v", asked)
		}
	}
	// A body that is not one object of a kind this removes.
	for _, body := range []string{
		``, `{}`, `{"kind":"Secret","name":"keycloak-admin","confirm":"keycloak-admin"}`,
		`{"kind":"Composition","name":"../x","confirm":"../x"}`,
		`{"kind":"ConfigMap","name":"a.b","namespace":"Not A Namespace","confirm":"a.b"}`,
		`{"kind":"Composition","names":["app-odoo","app-default"],"confirm":"app-odoo"}`,
		`{"kind":"ComponentProfile","name":"notes","namespace":"tenant-demo","confirm":"notes"}`,
	} {
		if code, out := h.do(t, "POST", removeResidue, alice, body); code != http.StatusBadRequest {
			t.Errorf("body %q: %d %v", body, code, out)
		}
	}
	if len(op.removals()) != 0 {
		t.Fatalf("a refused request reached the operator: %v", op.removals())
	}
	if h.tip(t) != before {
		t.Fatal("a refused request wrote to git")
	}
}

// A companion is removed by the operator, as the person named: one object,
// nothing committed, and the answer is the operator's account of it.
func TestACompanionIsRemovedByTheOperatorAsThePerson(t *testing.T) {
	op := &residueOperator{answers: []residueAnswer{
		deletedAnswer("ConfigMap", "shop.old-page"),
		refusedAnswer("not-residue", "the Composition app-default is not residue: it is the platform's own Composition. Nothing was deleted"),
		refusedAnswer("changed", "the Composition app-odoo was changed or removed by something else. Nothing was deleted by this request"),
	}}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)

	code, out := h.do(t, "POST", removeResidue, alice,
		`{"kind":"ConfigMap","name":"shop.old-page","namespace":"kernel-provisioning","confirm":"shop.old-page"}`)
	deleted, _ := out["deleted"].(map[string]any)
	if code != http.StatusAccepted || out["status"] != "deleted" || deleted["name"] != "shop.old-page" || out["message"] == "" {
		t.Fatalf("removal: %d %v", code, out)
	}
	want := `alice@example.com: {"kind":"ConfigMap","name":"shop.old-page","namespace":"kernel-provisioning"}`
	if got := op.removals(); len(got) != 1 || got[0] != want {
		t.Fatalf("the operator was asked %v, want %s", got, want)
	}

	// What the operator refuses is refused here in its words, with which
	// refusal it was.
	code, out = h.do(t, "POST", removeResidue, alice, `{"kind":"Composition","name":"app-default","confirm":"app-default"}`)
	if code != http.StatusConflict || out["reason"] != "not-residue" || !strings.Contains(fmt.Sprint(out["error"]), "the platform's own Composition") {
		t.Fatalf("a refusal: %d %v", code, out)
	}
	code, out = h.do(t, "POST", removeResidue, alice, `{"kind":"Composition","name":"app-odoo","confirm":"app-odoo"}`)
	if code != http.StatusConflict || out["reason"] != "changed" {
		t.Fatalf("a lost race: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("removing a companion wrote to git")
	}
}

const shopThemeBundle = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: shop
spec:
  classes: [app]
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop.theme
  labels:
    gentianos.io/profile-name: shop
    gentianos.io/asset: theme
data:
  theme.css: "body {}"
`

// An object a bundle file in the deployments repository declares is owned,
// whatever the cluster has caught up with: the operator is not even asked.
func TestACompanionGitStillDeclaresIsNotRemoved(t *testing.T) {
	op := &residueOperator{answers: []residueAnswer{deletedAnswer("ConfigMap", "shop.theme")}}
	h := startSeeded(t, op, func(remote string) {
		dt.Commit(t, remote, map[string]string{dt.CataloguePath("shop.yaml"): shopThemeBundle})
	})
	alice := h.token(t, "gentian", "alice")

	code, out := h.do(t, "POST", removeResidue, alice, `{"kind":"ConfigMap","name":"shop.theme","confirm":"shop.theme"}`)
	if code != http.StatusConflict || out["reason"] != "still-declared" || !strings.Contains(fmt.Sprint(out["error"]), "bundle of profile shop") {
		t.Fatalf("a declared companion: %d %v", code, out)
	}
	if len(op.removals()) != 0 {
		t.Fatalf("the operator was asked to delete what git declares: %v", op.removals())
	}
	// The same name of another kind is not what the file declares.
	if code, out := h.do(t, "POST", removeResidue, alice, `{"kind":"Customization","name":"shop.theme","confirm":"shop.theme"}`); code != http.StatusAccepted {
		t.Fatalf("another kind of the same name: %d %v", code, out)
	}
}

// installedThenUninstalled leaves notes-app as the director materialised it:
// in the catalogue directory, with its carrier, and in no tenant's manifest.
func installedThenUninstalled(t *testing.T, op api.Lifecycle) *harness {
	t.Helper()
	app := profileNamed("notes-app", "gentian")
	src := catalogues(t, served{"gentian": {"notes-app": app}})
	h := startSeeded(t, op, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "gentian", URL: src.URL + "/gentian"}),
			// The kustomization as the installer leaves it: there, and empty.
			dt.CataloguePath("kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n",
		})
	}, withFetcher(src))
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes-app", tom, fmt.Sprintf(`{"coordinate":"gentian/notes-app","digest":%q}`, sha(app))); code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, out)
	}
	if code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/notes-app", tom, ""); code != http.StatusAccepted {
		t.Fatalf("uninstall: %d %v", code, out)
	}
	return h
}

func inCatalogue(h *harness, file string) bool {
	_, err := tryRemoteFile(h, dt.CataloguePath(file))
	return err == nil
}

const removeNotes = `{"kind":"ComponentProfile","name":"notes-app","confirm":"notes-app"}`

// An unused profile leaves git in one commit by the person -- its bundle,
// the carrier and both kustomization entries -- and the operator is asked to
// delete the object until Argo CD has taken the commit in and it does.
func TestAnUnusedProfileLeavesGitAndThenTheCluster(t *testing.T) {
	defer api.SetResiduePoll(20 * time.Millisecond)()
	op := &residueOperator{unused: []string{"notes-app"}, answers: []residueAnswer{
		// Argo CD has not synced the commit yet, twice; then it has.
		refusedAnswer("still-declared", "Argo CD does not yet report the ComponentProfile notes-app as gone from the catalogue directory"),
		refusedAnswer("still-declared", "Argo CD does not yet report the ComponentProfile notes-app as gone from the catalogue directory"),
		{http.StatusOK, `{"status":"deleted","deleted":{"kind":"ComponentProfile","name":"notes-app","class":"unused-profile"},"message":"deleted"}`},
	}}
	h := installedThenUninstalled(t, op)
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)

	h.asked.reset()
	code, out := h.do(t, "POST", removeResidue, alice, removeNotes)
	if code != http.StatusAccepted || out["status"] != "committed" || out["deleted"] != nil || out["commit"] == nil ||
		!strings.Contains(fmt.Sprint(out["message"]), "still on the cluster") {
		t.Fatalf("removal: %d %v", code, out)
	}
	if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:alice can_configure cluster:"+dt.Cluster {
		t.Errorf("asked %v", asked)
	}
	// One commit, the person's, recording the decision.
	if h.tip(t) == before || h.tip(t) != out["commit"] {
		t.Fatalf("tip %s, answer names %v", h.tip(t), out["commit"])
	}
	log := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%s|%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(log, "Alice|feat(catalogue): remove unused profile notes-app") ||
		!strings.Contains(log, "can_configure cluster:"+dt.Cluster+" allowed") {
		t.Fatalf("commit = %q", log)
	}
	for _, file := range []string{"notes-app.yaml", gitops.BundleFile("notes-app")} {
		if inCatalogue(h, file) {
			t.Errorf("%s is still in the catalogue directory", file)
		}
	}
	k := dt.RemoteFile(t, h.remote, dt.CataloguePath("kustomization.yaml"))
	if strings.Contains(k, "notes-app") || strings.Contains(k, "patches:") || !strings.Contains(k, "resources: []") {
		t.Fatalf("the kustomization after the removal:\n%s", k)
	}
	// Every other profile of the directory is where it was.
	for _, name := range dt.OnCluster {
		if !inCatalogue(h, name+".yaml") {
			t.Errorf("%s.yaml went with it", name)
		}
	}

	// The operator is asked again until it deletes: three times in all.
	deadline := time.Now().Add(5 * time.Second)
	for len(op.removals()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the operator was asked %d times, want 3", len(op.removals()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	asked := op.removals()
	if len(asked) != 3 {
		t.Fatalf("asked again after it was deleted: %v", asked)
	}
	for _, one := range asked {
		if one != `alice@example.com: {"kind":"ComponentProfile","name":"notes-app"}` {
			t.Fatalf("the operator was asked %q", one)
		}
	}
}

// When Argo CD has the commit already -- the request is repeated, or the
// first one was cut short -- the answer is the deletion itself, and nothing
// more is committed.
func TestRepeatingTheRemovalOfAnUnusedProfileFinishesIt(t *testing.T) {
	defer api.SetResiduePoll(time.Hour)()
	op := &residueOperator{unused: []string{"notes-app"}, answers: []residueAnswer{
		refusedAnswer("still-declared", "not yet"),
		{http.StatusOK, `{"status":"deleted","deleted":{"kind":"ComponentProfile","name":"notes-app"},"message":"the ComponentProfile notes-app was deleted"}`},
	}}
	h := installedThenUninstalled(t, op)
	alice := h.token(t, "gentian", "alice")

	if code, out := h.do(t, "POST", removeResidue, alice, removeNotes); code != http.StatusAccepted || out["status"] != "committed" {
		t.Fatalf("first: %d %v", code, out)
	}
	tip := h.tip(t)
	code, out := h.do(t, "POST", removeResidue, alice, removeNotes)
	deleted, _ := out["deleted"].(map[string]any)
	if code != http.StatusAccepted || out["status"] != "deleted" || deleted["name"] != "notes-app" || out["commit"] != nil {
		t.Fatalf("second: %d %v", code, out)
	}
	if h.tip(t) != tip {
		t.Fatal("the repeated request committed again")
	}
}

// An unused profile stays, in git and so in the cluster, when the cluster
// does not list it as unused, when a tenant's manifest names it -- installed
// or as an add-on -- and when it is one the platform ships.
func TestAProfileSomebodyDependsOnIsNotRemoved(t *testing.T) {
	// The operator does not list it: a tenant retains data for it, say.
	op := &residueOperator{answers: []residueAnswer{deletedAnswer("ComponentProfile", "notes-app")}}
	h := installedThenUninstalled(t, op)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	code, out := h.do(t, "POST", removeResidue, alice, removeNotes)
	if code != http.StatusConflict || out["reason"] != "not-residue" || !strings.Contains(fmt.Sprint(out["error"]), "does not list") {
		t.Fatalf("not listed: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a profile the cluster does not list as unused was removed from git")
	}

	// The operator lists it, and git says a tenant has it installed: the
	// cluster has not caught up with an install made a moment ago.
	op.mu.Lock()
	op.unused = []string{"notes-app", "nextcloud", "calendar", "desktop"}
	op.mu.Unlock()
	if code, out := h.do(t, "POST", "/v1/tenants/solo/apps/notes-app", h.token(t, "tenant-solo", "tina"), ""); code != http.StatusAccepted {
		t.Fatalf("install in solo: %d %v", code, out)
	}
	before = h.tip(t)
	code, out = h.do(t, "POST", removeResidue, alice, removeNotes)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "tenant solo has it installed") {
		t.Fatalf("installed: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a profile a tenant has installed was removed from git")
	}

	// Switched on as an add-on.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar"]}`); code != http.StatusAccepted {
		t.Fatalf("add-on: %d %v", code, out)
	}
	before = h.tip(t)
	code, out = h.do(t, "POST", removeResidue, alice, `{"kind":"ComponentProfile","name":"calendar","confirm":"calendar"}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "switched on as an add-on of nextcloud") {
		t.Fatalf("add-on: %d %v", code, out)
	}
	code, out = h.do(t, "POST", removeResidue, alice, `{"kind":"ComponentProfile","name":"nextcloud","confirm":"nextcloud"}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "has it installed") {
		t.Fatalf("every fixture tenant has nextcloud: %d %v", code, out)
	}
	// One the platform ships.
	code, out = h.do(t, "POST", removeResidue, alice, `{"kind":"ComponentProfile","name":"desktop","confirm":"desktop"}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "the platform ships it") {
		t.Fatalf("a platform profile: %d %v", code, out)
	}

	if h.tip(t) != before {
		t.Fatal("a refused removal wrote to git")
	}
	if len(op.removals()) != 0 {
		t.Fatalf("the operator was asked to delete a profile somebody depends on: %v", op.removals())
	}
	for _, file := range []string{"notes-app.yaml", "calendar.yaml", "nextcloud.yaml"} {
		if !inCatalogue(h, file) {
			t.Errorf("%s was removed", file)
		}
	}
}
