/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package licencereport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	testSeed  = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	namespace = "kernel-control"
	storeApp  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	unsourced = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeCounter answers from fixed numbers, and fails for what it is told to.
type fakeCounter struct {
	tenants map[string]int
	apps    map[string]int
	failing map[string]bool
}

func (f *fakeCounter) TenantUsers(_ context.Context, t *gentianov1alpha1.Tenant) (int, error) {
	if f.failing[t.Name] {
		return 0, errors.New("keycloak did not answer")
	}
	return f.tenants[t.Name], nil
}

func (f *fakeCounter) AppUsers(_ context.Context, t *gentianov1alpha1.Tenant, profile string) (int, error) {
	if f.failing[t.Name+"/"+profile] {
		return 0, errors.New("keycloak did not answer")
	}
	return f.apps[t.Name+"/"+profile], nil
}

// endpoint is a receiving endpoint that keeps what it was sent.
type endpoint struct {
	*httptest.Server
	mu      sync.Mutex
	status  int
	bodies  [][]byte
	headers []http.Header
}

func newEndpoint(t *testing.T, status int) *endpoint {
	t.Helper()
	e := &endpoint{status: status}
	e.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		defer e.mu.Unlock()
		e.bodies = append(e.bodies, body)
		e.headers = append(e.headers, r.Header.Clone())
		w.WriteHeader(e.status)
		// An answer that tries to say something. Nothing reads it.
		_, _ = w.Write([]byte(`{"blocked":true,"disable":["nextcloud"]}`))
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *endpoint) answer(status int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status = status
}

func (e *endpoint) received() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.bodies)
}

// tenants is a cluster with a personal detail in every place one could come
// from: display names, administrators' addresses, realm names.
func tenants() []client.Object {
	deleting := metav1.NewTime(time.Unix(1, 0))
	return []client.Object{
		&gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "acme"},
			Spec: gentianov1alpha1.TenantSpec{
				DisplayName: "Acme Widgets of Zurich",
				Apps: []gentianov1alpha1.TenantApp{
					{Profile: "nextcloud-base-ee", Digest: storeApp, Catalogue: "gentian"},
					{Profile: "odoo-base-ee", Digest: unsourced},
					// Arrived with the kernel, or named with no build: not a
					// store install, and not reported.
					{Profile: "admin-console"},
					{Profile: "element"},
				},
			},
			Status: gentianov1alpha1.TenantStatus{AdminEmail: "ada.lovelace@acme.example"},
		},
		&gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "beta"},
			Spec:       gentianov1alpha1.TenantSpec{DisplayName: "Beta GmbH"},
			Status:     gentianov1alpha1.TenantStatus{Domain: "beta.example", AdminEmail: "bob@beta.example"},
		},
		&gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "gone", DeletionTimestamp: &deleting, Finalizers: []string{"x"}},
			Spec: gentianov1alpha1.TenantSpec{
				Apps: []gentianov1alpha1.TenantApp{{Profile: "nextcloud-base-ee", Digest: storeApp}},
			},
		},
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func counter() *fakeCounter {
	return &fakeCounter{
		tenants: map[string]int{"acme": 40, "beta": 3},
		apps:    map[string]int{"acme/nextcloud-base-ee": 25, "acme/odoo-base-ee": 4},
		failing: map[string]bool{},
	}
}

func reporter(t *testing.T, e *endpoint, c client.Client) *Reporter {
	t.Helper()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	r := &Reporter{
		Client: c, Reader: c, Namespace: namespace,
		Identity: Identity{ClusterID: "demo-cluster", KernelDomain: "k.example", TenancyMode: "multi"},
		Counter:  counter(),
		Key:      func() (*Key, error) { return KeyFromSeed(testSeed) },
		Now:      func() time.Time { return at },
	}
	if e != nil {
		r.Settings = Settings{Enabled: true, URL: e.URL + "/api/v1/licence-reports"}
		r.HTTP = e.Client()
	}
	return r
}

func record(t *testing.T, c client.Client) *Record {
	t.Helper()
	rec, err := readRecord(context.Background(), c, namespace)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// keysOf is every key an object carries, at every depth, as dotted paths with
// list positions left out.
func keysOf(v any, prefix string, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			into[path] = true
			keysOf(child, path, into)
		}
	case []any:
		for _, child := range x {
			keysOf(child, prefix, into)
		}
	}
}

// The body is counts and addresses of things. Its key set is asserted whole,
// so that a field added to it is a test somebody had to change on purpose,
// and no value anywhere in it is something a person is known by.
func TestTheBodyCarriesNoPersonalData(t *testing.T) {
	e := newEndpoint(t, http.StatusAccepted)
	c := newClient(t, tenants()...)
	if !reporter(t, e, c).RunOnce(context.Background(), time.Hour, time.Minute) {
		t.Fatalf("the report was not accepted: %+v", record(t, c).Attempt)
	}
	body := e.bodies[0]

	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	keysOf(parsed, "", got)
	want := []string{
		"version", "sequence", "sentAt", "publicKey",
		"cluster", "cluster.id", "cluster.url",
		"tenants", "tenants.url", "tenants.users",
		"tenants.apps", "tenants.apps.coordinate", "tenants.apps.digest", "tenants.apps.users",
	}
	var have []string
	for k := range got {
		have = append(have, k)
	}
	sort.Strings(have)
	sort.Strings(want)
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("the body's keys are\n %v\nwant exactly\n %v", have, want)
	}

	for _, personal := range []string{
		"Acme Widgets", "Beta GmbH", "ada.lovelace", "bob@", "@", "admin", "gentian:tenant", "members",
	} {
		if strings.Contains(string(body), personal) {
			t.Errorf("the body contains %q:\n%s", personal, body)
		}
	}

	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 || report.Sequence != 1 || report.SentAt != "2026-03-04T05:06:07Z" {
		t.Errorf("header = %+v", report)
	}
	if report.Cluster != (Cluster{ID: "demo-cluster", URL: "https://k.example"}) {
		t.Errorf("cluster = %+v", report.Cluster)
	}
	// A tenant on its way out is not reported, and each is named by its
	// address: its own domain where it has one.
	if len(report.Tenants) != 2 || report.Tenants[0].URL != "https://acme.k.example" || report.Tenants[1].URL != "https://beta.example" {
		t.Fatalf("tenants = %+v", report.Tenants)
	}
	if *report.Tenants[0].Users != 40 || *report.Tenants[1].Users != 3 {
		t.Errorf("accounts = %d, %d", *report.Tenants[0].Users, *report.Tenants[1].Users)
	}
}

// Only what was installed through the App Store is listed: an entry that
// carries a digest. The coordinate is given where the install recorded its
// catalogue, and is null where it did not.
func TestOnlyAppsThatCarryADigestAreListed(t *testing.T) {
	c := newClient(t, tenants()...)
	got, err := Build(context.Background(), c, counter(),
		Identity{ClusterID: "demo-cluster", KernelDomain: "k.example", TenancyMode: "multi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	apps := got[0].Apps
	if len(apps) != 2 {
		t.Fatalf("acme lists %d apps, want the two that carry a digest: %+v", len(apps), apps)
	}
	if apps[0].Digest != storeApp || apps[0].Coordinate == nil || *apps[0].Coordinate != "gentian/nextcloud-base-ee" || *apps[0].Users != 25 {
		t.Errorf("the store install = %+v", apps[0])
	}
	if apps[1].Digest != unsourced || apps[1].Coordinate != nil || *apps[1].Users != 4 {
		t.Errorf("the install with no catalogue recorded = %+v", apps[1])
	}
	// A tenant with nothing from the store says so with an empty list.
	if got[1].Apps == nil || len(got[1].Apps) != 0 {
		t.Errorf("beta's apps = %#v, want an empty list", got[1].Apps)
	}
	raw, _ := json.Marshal(got[0])
	if !strings.Contains(string(raw), `"coordinate":null`) {
		t.Errorf("an unknown coordinate is not sent as null: %s", raw)
	}
}

// A count that cannot be had is null, which is not the same as none, and the
// report still goes.
func TestACountThatCannotBeHadIsNull(t *testing.T) {
	e := newEndpoint(t, http.StatusOK)
	c := newClient(t, tenants()...)
	r := reporter(t, e, c)
	r.Counter.(*fakeCounter).failing = map[string]bool{"acme": true, "acme/odoo-base-ee": true}
	if !r.RunOnce(context.Background(), time.Hour, time.Minute) {
		t.Fatal("the report did not go")
	}
	body := string(e.bodies[0])
	if !strings.Contains(body, `{"url":"https://acme.k.example","users":null,`) {
		t.Errorf("uncounted accounts are not null:\n%s", body)
	}
	if !strings.Contains(body, `"digest":"`+unsourced+`","users":null}`) {
		t.Errorf("uncounted entitlements are not null:\n%s", body)
	}
	if !strings.Contains(body, `"users":25`) {
		t.Errorf("a count that could be had was dropped:\n%s", body)
	}
}

// The signature is over the request body byte for byte and verifies with the
// public key the body itself carries; the record holds the same bytes.
func TestTheSignatureVerifiesWithTheKeyInTheBody(t *testing.T) {
	e := newEndpoint(t, http.StatusAccepted)
	c := newClient(t, tenants()...)
	if !reporter(t, e, c).RunOnce(context.Background(), 24*time.Hour, time.Minute) {
		t.Fatal("the report was not accepted")
	}
	body, headers := e.bodies[0], e.headers[0]

	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	signature := headers.Get(SignatureHeader)
	if !strings.HasPrefix(signature, "ed25519=") {
		t.Fatalf("%s = %q", SignatureHeader, signature)
	}
	if !Verify(report.PublicKey, signature, body) {
		t.Fatal("the signature does not verify with the public key in the body")
	}
	tampered := []byte(strings.Replace(string(body), `"users":40`, `"users":4`, 1))
	if Verify(report.PublicKey, signature, tampered) {
		t.Fatal("the signature verifies a body that was changed")
	}
	key, _ := KeyFromSeed(testSeed)
	if headers.Get(KeyIDHeader) != key.ID() || len(key.ID()) != 16 {
		t.Errorf("%s = %q, want %q", KeyIDHeader, headers.Get(KeyIDHeader), key.ID())
	}
	if headers.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", headers.Get("Content-Type"))
	}
	// The private seed is in neither.
	if strings.Contains(string(body), testSeed) || strings.Contains(strings.Join(headers.Values(SignatureHeader), ""), testSeed) {
		t.Fatal("the private seed left the cluster")
	}

	// The record is the report exactly as sent, and its outcome.
	rec := record(t, c)
	if rec.Report == nil || rec.Report.Body != string(body) || rec.Report.Signature != signature || rec.Report.KeyID != key.ID() || rec.Report.Sequence != 1 {
		t.Fatalf("the record is not what was sent: %+v", rec.Report)
	}
	if rec.Attempt.Outcome != OutcomeAccepted || rec.Attempt.HTTPStatus != http.StatusAccepted ||
		rec.Attempt.At != "2026-03-04T05:06:07Z" || rec.Attempt.NextAt != "2026-03-05T05:06:07Z" {
		t.Fatalf("the outcome = %+v", rec.Attempt)
	}
	// And it is a ConfigMap, which is to say not a secret, holding no seed.
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: RecordConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	for k, v := range cm.Data {
		if strings.Contains(v, testSeed) {
			t.Fatalf("the record's %s holds the private seed", k)
		}
	}
}

// Off, or on with nowhere to report to, nothing is sent and nothing is
// recorded. An address that is not https is nowhere.
func TestDisabledOrWithoutAnAddressNothingIsSent(t *testing.T) {
	e := newEndpoint(t, http.StatusOK)
	plain := "http://" + strings.TrimPrefix(e.URL, "https://")
	for name, settings := range map[string]Settings{
		"disabled":         {Enabled: false, URL: e.URL},
		"no address":       {Enabled: true, URL: ""},
		"disabled, no url": {},
		"not https":        {Enabled: true, URL: plain},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, tenants()...)
			r := reporter(t, e, c)
			r.Settings = settings
			if settings.Active() {
				t.Fatal("these settings count as reporting")
			}
			if r.RunOnce(context.Background(), time.Hour, time.Minute) {
				t.Fatal("an attempt was reported as sent")
			}
			// Start waits for the end and does nothing else.
			ctx, cancel := context.WithCancel(context.Background())
			r.InitialDelay, r.Interval = time.Millisecond, time.Millisecond
			done := make(chan error, 1)
			go func() { done <- r.Start(ctx) }()
			time.Sleep(50 * time.Millisecond)
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if e.received() != 0 {
				t.Fatalf("%d requests reached the endpoint", e.received())
			}
			if rec := record(t, c); rec.Attempt != nil || rec.Report != nil {
				t.Fatalf("something was recorded: %+v", rec)
			}
			view, err := Read(context.Background(), c, namespace, settings)
			raw, _ := json.Marshal(view)
			if err != nil || string(raw) != `{"enabled":false}` {
				t.Fatalf("the answer = %s, %v", raw, err)
			}
		})
	}
}

// Without the key no report goes unsigned: nothing is sent, the record says
// why, and the attempt is made again once the key has arrived.
func TestWithoutTheKeyNothingIsSentAndTheRecordSaysWhy(t *testing.T) {
	e := newEndpoint(t, http.StatusOK)
	c := newClient(t, tenants()...)
	r := reporter(t, e, c)
	path := filepath.Join(t.TempDir(), "signing_seed")
	r.Key = func() (*Key, error) { return KeyFromFile(path) }

	if r.RunOnce(context.Background(), time.Hour, 2*time.Minute) {
		t.Fatal("a report was sent without a key")
	}
	if e.received() != 0 {
		t.Fatal("a request reached the endpoint without a signature")
	}
	rec := record(t, c)
	if rec.Report != nil || rec.Attempt == nil || rec.Attempt.Outcome != OutcomeNotSent || rec.Attempt.Reason != ReasonKeyAbsent ||
		rec.Attempt.NextAt != "2026-03-04T05:08:07Z" {
		t.Fatalf("the record = %+v %+v", rec.Attempt, rec.Report)
	}

	// A key that is there and is not a key is said to be invalid, and its
	// content is not quoted.
	if err := os.WriteFile(path, []byte("not-a-seed-and-rather-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r.RunOnce(context.Background(), time.Hour, time.Minute) || e.received() != 0 {
		t.Fatal("a report was sent with a key that is not one")
	}
	if rec := record(t, c); rec.Attempt.Reason != ReasonKeyInvalid || strings.Contains(rec.Attempt.Error, "rather-secret") {
		t.Fatalf("the record = %+v", rec.Attempt)
	}

	if err := os.WriteFile(path, []byte(testSeed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !r.RunOnce(context.Background(), time.Hour, time.Minute) || e.received() != 1 {
		t.Fatal("the key arrived and no report followed")
	}
}

// A report that does not arrive is recorded as it was sent, with what went
// wrong and when it is tried again; the next one carries the next number.
func TestAFailureIsRecordedAndRetried(t *testing.T) {
	e := newEndpoint(t, http.StatusServiceUnavailable)
	c := newClient(t, tenants()...)
	r := reporter(t, e, c)
	ctx := context.Background()

	if r.RunOnce(ctx, 24*time.Hour, 4*time.Minute) {
		t.Fatal("a refused report counted as sent")
	}
	rec := record(t, c)
	if rec.Attempt.Outcome != OutcomeFailed || rec.Attempt.Reason != ReasonRefused || rec.Attempt.HTTPStatus != 503 ||
		rec.Attempt.NextAt != "2026-03-04T05:10:07Z" {
		t.Fatalf("the attempt = %+v", rec.Attempt)
	}
	if rec.Report == nil || rec.Report.Sequence != 1 || rec.Report.Body != string(e.bodies[0]) {
		t.Fatalf("the record does not hold what was sent: %+v", rec.Report)
	}

	// An endpoint that cannot be reached at all.
	e.CloseClientConnections()
	down := *r
	down.Settings.URL = "https://127.0.0.1:1/unreachable"
	if down.RunOnce(ctx, 24*time.Hour, time.Minute) {
		t.Fatal("an unreachable endpoint counted as sent")
	}
	rec = record(t, c)
	if rec.Attempt.Outcome != OutcomeFailed || rec.Attempt.Reason != ReasonUnreachable || rec.Attempt.Error == "" || rec.Report.Sequence != 2 {
		t.Fatalf("the attempt = %+v, sequence %d", rec.Attempt, rec.Report.Sequence)
	}

	// The schedule: the wait doubles from the minimum to the maximum, and a
	// failing endpoint is asked again without anything else stopping.
	if a, b, top := r.backoff(0), r.backoff(time.Minute), r.backoff(45*time.Minute); a != time.Minute || b != 2*time.Minute || top != time.Hour {
		t.Fatalf("backoff = %v, %v, %v", a, b, top)
	}
	loop := *r
	loop.InitialDelay, loop.Interval, loop.RetryMin, loop.RetryMax = time.Millisecond, time.Hour, time.Millisecond, 4*time.Millisecond
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- loop.Start(loopCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.received() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// It is accepted at last, and then the loop waits out the interval.
	e.answer(http.StatusNoContent)
	for record(t, c).Attempt.Outcome != OutcomeAccepted && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	settled := e.received()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the reporter returned an error to the manager: %v", err)
	}
	if settled < 4 || e.received() != settled {
		t.Fatalf("retries = %d, after acceptance %d", settled, e.received())
	}

	// Every report had its own number, in order.
	var last int64
	for i, body := range e.bodies {
		var report Report
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		if report.Sequence <= last {
			t.Fatalf("report %d has sequence %d after %d", i, report.Sequence, last)
		}
		last = report.Sequence
	}
}

// A report that cannot be recorded is not sent: what left the cluster is
// always something the cluster can show.
func TestWhatCannotBeRecordedIsNotSent(t *testing.T) {
	e := newEndpoint(t, http.StatusOK)
	c := newClient(t, tenants()...)
	r := reporter(t, e, c)
	r.Client = refusingWrites{c}
	if r.RunOnce(context.Background(), time.Hour, time.Minute) || e.received() != 0 {
		t.Fatalf("a report went out unrecorded (%d requests)", e.received())
	}

	// A stored sequence that is not a number is not restarted from one.
	bad := newClient(t, append(tenants(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: RecordConfigMap},
		Data:       map[string]string{keySequence: "many"},
	})...)
	if reporter(t, e, bad).RunOnce(context.Background(), time.Hour, time.Minute) || e.received() != 0 {
		t.Fatal("a report was sent over a sequence that cannot be continued")
	}
}

type refusingWrites struct{ client.Client }

func (refusingWrites) Create(context.Context, client.Object, ...client.CreateOption) error {
	return errors.New("forbidden")
}

func (refusingWrites) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errors.New("forbidden")
}

// A redirect is not followed: the address was fixed at install time.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := newEndpoint(t, http.StatusOK)
	front := httptest.NewTLSServer(http.RedirectHandler(elsewhere.URL, http.StatusTemporaryRedirect))
	t.Cleanup(front.Close)
	c := newClient(t, tenants()...)
	r := reporter(t, elsewhere, c)
	r.Settings.URL = front.URL
	r.HTTP = front.Client()
	r.HTTP.CheckRedirect = r.clientWithout().CheckRedirect
	if r.RunOnce(context.Background(), time.Hour, time.Minute) || elsewhere.received() != 0 {
		t.Fatal("the body followed a redirect to another host")
	}
	if rec := record(t, c); rec.Attempt.HTTPStatus != http.StatusTemporaryRedirect || rec.Attempt.Reason != ReasonRefused {
		t.Fatalf("the attempt = %+v", rec.Attempt)
	}
}

func TestSettingsComeFromTheEnvironmentAndDefaultToOff(t *testing.T) {
	t.Setenv("LICENCE_REPORT_ENABLED", "")
	t.Setenv("LICENCE_REPORT_URL", "")
	if SettingsFromEnv().Active() {
		t.Fatal("an unset environment reports")
	}
	t.Setenv("LICENCE_REPORT_URL", "https://reports.example/v1")
	if SettingsFromEnv().Active() {
		t.Fatal("an address alone turns reporting on")
	}
	t.Setenv("LICENCE_REPORT_ENABLED", "true")
	if s := SettingsFromEnv(); !s.Active() || s.URL != "https://reports.example/v1" {
		t.Fatalf("settings = %+v", s)
	}
}
