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

package membership_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/membership"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var scope = membership.Scope{PlatformRealm: "kernel", PlatformTenant: "platform"}

// ---- harness --------------------------------------------------------------

type listener struct {
	t     *testing.T
	url   string
	keyID string
	key   ed25519.PrivateKey
	n     int
}

func (l *listener) event(ev membership.Event) membership.Event {
	l.n++
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("event-%06d", l.n)
	}
	if ev.Time == 0 {
		ev.Time = time.Now().UnixMilli() + int64(l.n)
	}
	return ev
}

func (l *listener) send(ev membership.Event) int {
	return l.sendAt(l.event(ev), time.Now(), nil)
}

func (l *listener) sendAt(ev membership.Event, at time.Time, tamper func(body []byte) []byte) int {
	l.t.Helper()
	body, _ := json.Marshal(ev)
	ts := fmt.Sprint(at.Unix())
	sig := ed25519.Sign(l.key, append([]byte(ts+"."), body...))
	if tamper != nil {
		body = tamper(body)
	}
	req, _ := http.NewRequest("POST", l.url, bytes.NewReader(body))
	req.Header.Set(membership.SignatureHeader, "keyid="+l.keyID+",t="+ts+",sig="+base64.StdEncoding.EncodeToString(sig))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func start(t *testing.T, store membership.Store) *listener {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	proj, err := membership.NewProjector(store, scope, quiet)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := membership.NewReceiver(map[string]ed25519.PublicKey{"k1": pub}, proj, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(rc)
	t.Cleanup(ts.Close)
	return &listener{t: t, url: ts.URL, keyID: "k1", key: priv}
}

func groupsOf(t *testing.T, store membership.Store, user string) string {
	t.Helper()
	tuples, err := store.Read(context.Background(), authz.Tuple{User: "user:" + user, Object: "group:"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tu := range tuples {
		out = append(out, strings.TrimPrefix(tu.Object, "group:"))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// ---- behaviour ------------------------------------------------------------

func TestAnEventMakesTheStoreMatchKeycloak(t *testing.T) {
	store := newStore(t)
	l := start(t, store)

	if code := l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-tom",
		Groups: []string{"/gentian:tenant:demo:admins", "gentian:tenant:demo:members", "book-club"}}); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if got := groupsOf(t, store, "u-tom"); got != "gentian/tenant/demo/admins gentian/tenant/demo/members" {
		t.Fatalf("groups = %q", got)
	}

	// The next statement is the whole truth: what it omits is gone.
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-tom",
		Groups: []string{"gentian:tenant:demo:members", "gentian:tenant:demo:app:nextcloud"}})
	if got := groupsOf(t, store, "u-tom"); got != "gentian/tenant/demo/app/nextcloud gentian/tenant/demo/members" {
		t.Fatalf("groups = %q", got)
	}

	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserDeleted, User: "u-tom"})
	if got := groupsOf(t, store, "u-tom"); got != "" {
		t.Fatalf("a deleted user still holds %q", got)
	}
}

// A tenant's administrators can create any group in their own realm. The name
// alone must not be enough to hold the role it spells.
func TestARealmSpeaksOnlyForItsOwnTenant(t *testing.T) {
	store := newStore(t)
	l := start(t, store)

	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-mallory", Groups: []string{
		"gentian:platform:admin", "gentian:platform:break-glass",
		"gentian:tenant:other:admins", "gentian:tenant:platform:admins",
		"gentian:tenant:demo:members",
	}})
	if got := groupsOf(t, store, "u-mallory"); got != "gentian/tenant/demo/members" {
		t.Fatalf("groups = %q", got)
	}

	l.send(membership.Event{Realm: "kernel", Type: membership.TypeUserMemberships, User: "u-alice", Groups: []string{
		"gentian:platform:admin", "gentian:tenant:platform:admins", "gentian:tenant:demo:admins",
	}})
	if got := groupsOf(t, store, "u-alice"); got != "gentian/platform/admin gentian/tenant/platform/admins" {
		t.Fatalf("groups = %q", got)
	}
}

// Nor may one realm's statement remove what another realm asserted.
func TestARealmRemovesOnlyItsOwnAssertions(t *testing.T) {
	store := newStore(t)
	l := start(t, store)
	l.send(membership.Event{Realm: "kernel", Type: membership.TypeUserMemberships, User: "u-same", Groups: []string{"gentian:platform:auditor"}})
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-same", Groups: []string{"gentian:tenant:demo:members"}})
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserDeleted, User: "u-same"})
	if got := groupsOf(t, store, "u-same"); got != "gentian/platform/auditor" {
		t.Fatalf("groups = %q", got)
	}
}

func TestADeletedGroupLosesItsMembers(t *testing.T) {
	store := newStore(t)
	l := start(t, store)
	for _, u := range []string{"u-1", "u-2", "u-3"} {
		l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: u,
			Groups: []string{"gentian:tenant:demo:perimeter", "gentian:tenant:demo:members"}})
	}
	if code := l.send(membership.Event{Realm: "other", Type: membership.TypeGroupDeleted, Group: "gentian:tenant:demo:perimeter"}); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if got := groupsOf(t, store, "u-1"); !strings.Contains(got, "perimeter") {
		t.Fatal("another realm deleted this tenant's group")
	}
	l.send(membership.Event{Realm: "demo", Type: membership.TypeGroupDeleted, Group: "gentian:tenant:demo:perimeter"})
	for _, u := range []string{"u-1", "u-2", "u-3"} {
		if got := groupsOf(t, store, u); got != "gentian/tenant/demo/members" {
			t.Fatalf("%s: groups = %q", u, got)
		}
	}
}

func TestOnlyTheListenerIsBelieved(t *testing.T) {
	store := newStore(t)
	l := start(t, store)
	ev := membership.Event{Realm: "kernel", Type: membership.TypeUserMemberships, User: "u-eve", Groups: []string{"gentian:platform:admin"}}

	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]func() int{
		"no signature": func() int {
			body, _ := json.Marshal(l.event(ev))
			resp, err := http.Post(l.url, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			return resp.StatusCode
		},
		"another key": func() int {
			forged := *l
			forged.key = stranger
			return forged.sendAt(l.event(ev), time.Now(), nil)
		},
		"unknown key id": func() int {
			forged := *l
			forged.keyID = "k9"
			return forged.sendAt(l.event(ev), time.Now(), nil)
		},
		"body changed after signing": func() int {
			return l.sendAt(l.event(membership.Event{Realm: "kernel", Type: membership.TypeUserMemberships, User: "u-eve",
				Groups: []string{"gentian:platform:auditor"}}), time.Now(),
				func(b []byte) []byte { return bytes.Replace(b, []byte("auditor"), []byte("admin"), 1) })
		},
		"captured an hour ago": func() int { return l.sendAt(l.event(ev), time.Now().Add(-time.Hour), nil) },
		"dated an hour ahead":  func() int { return l.sendAt(l.event(ev), time.Now().Add(time.Hour), nil) },
	}
	for name, attempt := range cases {
		if code := attempt(); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, code)
		}
	}
	if got := groupsOf(t, store, "u-eve"); got != "" {
		t.Fatalf("an unauthenticated event wrote %q", got)
	}
}

// A captured grant, replayed after the removal that followed it, must not
// bring the membership back.
func TestAReplayDoesNotUndoARemoval(t *testing.T) {
	store := newStore(t)
	l := start(t, store)
	grant := l.event(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-rob", Groups: []string{"gentian:tenant:demo:admins"}})
	l.sendAt(grant, time.Now(), nil)
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-rob"})
	if got := groupsOf(t, store, "u-rob"); got != "" {
		t.Fatalf("groups = %q", got)
	}

	if code := l.sendAt(grant, time.Now(), nil); code != http.StatusNoContent {
		t.Fatalf("replay: status %d", code)
	}
	// The same statement under a fresh id is still older than what is held.
	again := grant
	again.ID = "event-renamed-1"
	l.sendAt(again, time.Now(), nil)
	if got := groupsOf(t, store, "u-rob"); got != "" {
		t.Fatalf("the replay restored %q", got)
	}
}

// When the store is down the listener is told so, and its retry is applied
// rather than mistaken for a replay.
func TestAnEventThatCouldNotBeAppliedCanBeRetried(t *testing.T) {
	mem := newMemory()
	flaky := &failing{Store: mem, fail: true}
	l := start(t, flaky)
	ev := l.event(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-ann", Groups: []string{"gentian:tenant:demo:members"}})
	if code := l.sendAt(ev, time.Now(), nil); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", code)
	}
	flaky.fail = false
	if code := l.sendAt(ev, time.Now(), nil); code != http.StatusNoContent {
		t.Fatalf("retry: status %d", code)
	}
	if got := groupsOf(t, mem, "u-ann"); got != "gentian/tenant/demo/members" {
		t.Fatalf("groups = %q", got)
	}
}

// With a real OpenFGA, the point of all this: the event is what makes a Check pass.
func TestAMembershipEventChangesADecision(t *testing.T) {
	base := os.Getenv("DIRECTOR_TEST_OPENFGA_URL")
	if base == "" {
		t.Skip("needs OpenFGA: make test-director-contract")
	}
	fga := newOpenFGA(t, base)
	ctx := context.Background()
	if err := fga.Write(ctx, []authz.Tuple{{User: "group:gentian/tenant/demo/admins#member", Relation: "admin", Object: "tenant:demo"}}, nil); err != nil {
		t.Fatal(err)
	}
	l := start(t, fga)
	may := func() bool {
		ok, err := fga.Check(ctx, "test", "user:u-tom", "can_install_app", "tenant:demo")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if may() {
		t.Fatal("allowed before any membership")
	}
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-tom", Groups: []string{"gentian:tenant:demo:admins"}})
	if !may() {
		t.Fatal("denied after the grant")
	}
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-tom"})
	if may() {
		t.Fatal("still allowed after the removal")
	}
}

// ---- stores ---------------------------------------------------------------

func newStore(t *testing.T) membership.Store {
	if base := os.Getenv("DIRECTOR_TEST_OPENFGA_URL"); base != "" {
		return newOpenFGA(t, base)
	}
	return newMemory()
}

func newOpenFGA(t *testing.T, base string) *authz.OpenFGA {
	t.Helper()
	post := func(path string, body any, out any) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("openfga %s: %d %s", path, resp.StatusCode, raw)
		}
		_ = json.Unmarshal(raw, out)
	}
	var store struct {
		ID string `json:"id"`
	}
	post("/stores", map[string]string{"name": t.Name()}, &store)
	raw, err := os.ReadFile("../../../authz/model/v1/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var model map[string]any
	_ = json.Unmarshal(raw, &model)
	var written struct {
		ID string `json:"authorization_model_id"`
	}
	post("/stores/"+store.ID+"/authorization-models", model, &written)
	c, err := authz.NewOpenFGA(authz.Options{BaseURL: base, StoreID: store.ID, ModelID: written.ID, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// memory is a Store with OpenFGA's write semantics: writing a tuple that
// exists, or deleting one that does not, fails the request.
type memory struct {
	mu     sync.Mutex
	tuples map[authz.Tuple]bool
}

func newMemory() *memory { return &memory{tuples: map[authz.Tuple]bool{}} }

func (m *memory) Read(_ context.Context, f authz.Tuple) ([]authz.Tuple, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []authz.Tuple
	for t := range m.tuples {
		if (f.User == "" || f.User == t.User) && (f.Relation == "" || f.Relation == t.Relation) &&
			(f.Object == "" || f.Object == t.Object || (strings.HasSuffix(f.Object, ":") && strings.HasPrefix(t.Object, f.Object))) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memory) Write(_ context.Context, writes, deletes []authz.Tuple) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range deletes {
		if !m.tuples[t] {
			return fmt.Errorf("tuple to delete does not exist: %v", t)
		}
	}
	for _, t := range writes {
		if m.tuples[t] {
			return fmt.Errorf("tuple already exists: %v", t)
		}
	}
	for _, t := range deletes {
		delete(m.tuples, t)
	}
	for _, t := range writes {
		m.tuples[t] = true
	}
	return nil
}

type failing struct {
	membership.Store
	fail bool
}

func (f *failing) Read(ctx context.Context, t authz.Tuple) ([]authz.Tuple, error) {
	if f.fail {
		return nil, errors.New("connection refused")
	}
	return f.Store.Read(ctx, t)
}
