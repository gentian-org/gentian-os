/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// Every object the operator writes for mail, and who reads it.
//
// The reader is stated from what the repository deploys, not from the
// operator's own constants: the chart whose pod mounts the object and the
// namespace the mail ApplicationSet syncs that chart into, the Job that takes
// it as an environment variable and the namespace that Job runs in, the
// Composition that names it. The operator is then run against an empty
// cluster and each object has to turn up in its reader's namespace and in no
// other.
//
// Two objects were written where nothing read them -- Postfix's maps in the
// mail DMZ, Dovecot's passwd-files in the edge namespace -- and both mounts
// were optional, so nothing failed: Postfix knew no tenant domain and Dovecot
// no password, while each tenant reported its mail as ready. A row here is
// what a third one would have to get past.
type mailObjectReader struct {
	kind string // ConfigMap or Secret
	name string
	// The chart of kernel/services whose templates name the object, when a
	// mail server mounts it.
	chart string
	// The namespace of the reader when it is not a chart of the mail
	// ApplicationSet: the operator itself, a Job, a Composition.
	namespace string
	why       string
}

const pinTenant = "pinned"

func mailObjectReaders() []mailObjectReader {
	identity := layout.Namespace(layout.Authentication)
	return []mailObjectReader{
		{kind: "ConfigMap", name: "postfix-kernel-virtual-mailbox-maps", chart: "postfix",
			why: "Postfix's pod mounts it at /etc/postfix/kernel-maps and takes mynetworks and the sender domains from it"},
		{kind: "Secret", name: "postfix-dkim-tenants", chart: "postfix",
			why: "Postfix's pod mounts the signing tables and keys"},
		{kind: "Secret", name: "dovecot-app-passwords", chart: "dovecot",
			why: "Dovecot's pod mounts the passwd-files at /etc/dovecot/apppw"},
		{kind: "ConfigMap", name: "mail-postfix-virtual-domains", namespace: layout.System("mail"),
			why: "the operator's own registry of tenant domains, kept beside what is derived from it"},
		{kind: "ConfigMap", name: "mail-dovecot-domains", namespace: layout.System("mail"),
			why: "the operator's own record of which tenant domains have mailboxes; no pod mounts it"},
		{kind: "Secret", name: "dkim-" + pinTenant, namespace: layout.System("mail"),
			why: "the operator's own copy of the tenant's signing key, in the namespace the signer is in"},
		{kind: "Secret", name: "dkim-kernel", namespace: layout.System("mail"),
			why: "the same, for the cluster's own domain"},
		{kind: "Secret", name: "mail-apppw-seed-" + pinTenant, namespace: layout.System("mail"),
			why: "the operator alone reads the seed; it stays beside the hashes it yields, out of the edge"},
		{kind: "Secret", name: "mail-submission-" + pinTenant, namespace: identity,
			why: "the realm's SMTP Job and the tenant's Composition read it beside Keycloak"},
		{kind: "Secret", name: "mail-app-passwords", namespace: "tenant-" + pinTenant,
			why: "the tenant's mail client reads its users' passwords in the tenant's own namespace"},
		{kind: "Secret", name: "smtp-credentials-" + pinTenant, namespace: "tenant-" + pinTenant,
			why: "the tenant's apps read their submission credential in the tenant's own namespace"},
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}

// mailChartNamespaces reads, from the mail ApplicationSet, which namespace
// each chart is synced into: the function its placeholder names, resolved by
// the layout the way the ApplicationSet's wrapper resolves it.
func mailChartNamespaces(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "kernel/appsets/raw/09a-mail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	out := map[string]string{}
	// One element per chart: `- app: <chart>` followed by the namespace it is
	// synced into, as a placeholder the wrapper resolves from the layout.
	element := regexp.MustCompile(`(?m)^\s*- app: (\S+)\n\s*namespace: sys\.([a-z-]+)\.placeholder$`)
	for _, m := range element.FindAllStringSubmatch(text, -1) {
		out[m[1]] = layout.System(m[2])
	}
	if len(out) == 0 {
		t.Fatal("the mail ApplicationSet names no chart with its namespace")
	}
	if !strings.Contains(text, "namespace: '{{.namespace}}'") {
		t.Fatal("the mail ApplicationSet no longer syncs each chart into the namespace its element names")
	}
	return out
}

func chartText(t *testing.T, chart string) string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "kernel/services", chart, "manifests/templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("chart %s: %v", chart, err)
	}
	var b strings.Builder
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}

// keycloakForTest answers the two calls the operator makes to list a realm's
// users.
func keycloakForTest(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t"})
		case strings.HasSuffix(r.URL.Path, "/users"):
			if r.URL.Query().Get("first") != "0" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"username": "ada", "email": "ada@example.test", "enabled": true}})
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mailPinCluster(t *testing.T) (*controller.TenantReconciler, client.Client, *gentianov1alpha1.Tenant) {
	t.Helper()
	identity := layout.Namespace(layout.Authentication)
	kc := keycloakForTest(t)
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: layout.System("mail")}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: layout.System("mail-dmz")}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: identity}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-" + pinTenant}},
		// Beside Keycloak, where its ExternalSecret writes it. The operator
		// has to look for it here: nothing writes one anywhere else.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "keycloak-admin", Namespace: identity},
			Data:       map[string][]byte{"url": []byte(kc.URL), "client_id": []byte("admin"), "client_secret": []byte("x")},
		},
		// The kernel realm's submission credential, written by the installer
		// beside Keycloak.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "keycloak-smtp-credentials", Namespace: identity},
			Data:       map[string][]byte{"smtp_user": []byte("gentian-system@example.test"), "smtp_password": []byte("pw")},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: pinTenant},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Pinned Co",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeSelfhosted},
		},
	}
	objects = append(objects, tenant)
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(objects...).Build()
	r := &controller.TenantReconciler{Client: c, MailServiceMode: "system", KernelDomain: "example.test", KernelRealm: "kernel"}
	return r, c, tenant
}

func where(t *testing.T, c client.Client, kind, name string) []string {
	t.Helper()
	var found []string
	switch kind {
	case "ConfigMap":
		list := &corev1.ConfigMapList{}
		if err := c.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		for _, o := range list.Items {
			if o.Name == name {
				found = append(found, o.Namespace)
			}
		}
	case "Secret":
		list := &corev1.SecretList{}
		if err := c.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		for _, o := range list.Items {
			if o.Name == name {
				found = append(found, o.Namespace)
			}
		}
	default:
		t.Fatalf("unknown kind %s", kind)
	}
	sort.Strings(found)
	return found
}

func TestMailObjects_AreWrittenWhereTheirReaderIs(t *testing.T) {
	t.Parallel()
	r, c, tenant := mailPinCluster(t)
	ctx := context.Background()

	// The tenant's whole mail provisioning. It stops short of ready -- no
	// Keycloak client exists for Dovecot in a fake cluster -- after every
	// object of the table has been written.
	_ = r.EnsureMailForTest(ctx, tenant)
	if err := r.SyncMailAppPasswordsForTest(ctx, tenant); err != nil {
		t.Fatalf("mail passwords: %v", err)
	}
	if err := r.SyncKernelRealmSubmissionIdentityForTest(ctx); err != nil {
		t.Fatalf("kernel realm submission identity: %v", err)
	}
	if err := r.SyncPostfixDKIMTablesForTest(ctx); err != nil {
		t.Fatalf("signing tables: %v", err)
	}

	chartNamespaces := mailChartNamespaces(t)
	known := map[string]bool{}
	for _, o := range mailObjectReaders() {
		known[o.kind+"/"+o.name] = true
		want := o.namespace
		if o.chart != "" {
			ns, ok := chartNamespaces[o.chart]
			if !ok {
				t.Errorf("%s %s: the mail ApplicationSet deploys no chart %q", o.kind, o.name, o.chart)
				continue
			}
			want = ns
			// The chart really is the reader: its templates name the object.
			if !regexp.MustCompile(`(?m)^\s*(name|secretName): ` + regexp.QuoteMeta(o.name) + `\s*$`).MatchString(chartText(t, o.chart)) {
				t.Errorf("%s %s: no template of the %s chart mounts it, so the chart is not its reader", o.kind, o.name, o.chart)
			}
		}
		got := where(t, c, o.kind, o.name)
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s %s is written to %v; its reader is in %s (%s)", o.kind, o.name, got, want, o.why)
		}
	}

	// Nothing the operator wrote for mail is outside the table: an object
	// added to the writer and not here has no reader anybody stated.
	for _, kind := range []string{"ConfigMap", "Secret"} {
		for _, name := range writtenNames(t, c, kind) {
			if name == "keycloak-admin" || name == "keycloak-smtp-credentials" {
				continue // the test's own, read by the operator
			}
			if !known[kind+"/"+name] {
				t.Errorf("%s %s was written for mail and has no row saying who reads it and where", kind, name)
			}
		}
	}

	// The hashes hold every identity that presents a password, the kernel
	// realm's among them -- which was read from a namespace it is not in.
	hashes := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "dovecot-app-passwords", Namespace: chartNamespaces["dovecot"]}, hashes); err != nil {
		t.Fatalf("Dovecot's passwd-files: %v", err)
	}
	for _, key := range []string{
		"nextcloud-mail-" + pinTenant + ".users", "keycloak-smtp-" + pinTenant + ".users",
		"tenant-apps-" + pinTenant + ".users", "kernel-realm-kernel.users",
	} {
		if len(hashes.Data[key]) == 0 {
			t.Errorf("Dovecot's passwd-files lack %s", key)
		}
	}
}

func writtenNames(t *testing.T, c client.Client, kind string) []string {
	t.Helper()
	seen := map[string]bool{}
	if kind == "ConfigMap" {
		list := &corev1.ConfigMapList{}
		if err := c.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		for _, o := range list.Items {
			seen[o.Name] = true
		}
	} else {
		list := &corev1.SecretList{}
		if err := c.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		for _, o := range list.Items {
			seen[o.Name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// The Job that configures a realm's mail takes the submission credential
// from its own namespace, and the Composition that declares the realm names
// the same one. Both are beside Keycloak.
func TestMailObjects_TheRealmReadsItsSubmissionCredentialBesideKeycloak(t *testing.T) {
	t.Parallel()
	identity := layout.Namespace(layout.Authentication)
	job := controller.TenantSMTPJobForTest(pinTenant)
	if job.Namespace != identity {
		t.Fatalf("the realm's SMTP Job runs in %s, not beside Keycloak in %s", job.Namespace, identity)
	}
	reads := map[string]bool{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			reads[e.ValueFrom.SecretKeyRef.Name] = true
		}
	}
	for _, name := range []string{"mail-submission-" + pinTenant, "keycloak-smtp-credentials"} {
		if !reads[name] {
			t.Errorf("the realm's SMTP Job no longer reads %s; find where it is read now and hold that", name)
		}
	}

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "crossplane/compositions/tenant-default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The Composition takes the Secret's name from spec.realmMail, which the
	// operator states (TestMail_RealmMailIsStatedOnceItsPasswordExists holds
	// the name), and the namespace is the Composition's own to say.
	ref := regexp.MustCompile(`name: \{\{ \$rm\.passwordSecretRef\.name \| quote \}\}\n\s*namespace: \{\{ (\$\w+) \}\}`).FindStringSubmatch(string(raw))
	if ref == nil {
		t.Fatal("the tenant Composition no longer names the Secret of the realm's mail password")
	}
	if ref[1] != "$identityNs" {
		t.Errorf("the tenant Composition reads the realm's submission Secret from %s; the operator writes it beside Keycloak ($identityNs)", ref[1])
	}
	if !strings.Contains(string(raw), `$identityNs  := default "`+identity+`"`) {
		t.Errorf("the tenant Composition's $identityNs no longer defaults to %s", identity)
	}
}

// What an earlier version wrote where nothing read it is gone after a start,
// and what cannot be derived again has moved rather than been made anew.
func TestMailObjects_MisplacedOnesAreRetired(t *testing.T) {
	t.Parallel()
	r, c, _ := mailPinCluster(t)
	ctx := context.Background()
	edge := controller.ServicesNamespaceForTest()
	managed := map[string]string{"app.kubernetes.io/managed-by": "gentian-os"}
	seed := []byte("0123456789abcdef0123456789abcdef")
	for _, o := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "postfix-kernel-virtual-mailbox-maps", Namespace: layout.System("mail-dmz"), Labels: managed}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "dovecot-app-passwords", Namespace: edge, Labels: managed}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mail-apppw-seed-" + pinTenant, Namespace: edge, Labels: managed}, Data: map[string][]byte{"seed": seed}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mail-submission-" + pinTenant, Namespace: edge, Labels: managed}, Data: map[string][]byte{"smtp_password": []byte("p")}},
		// Not the operator's: same name, no label. Left alone.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mail-submission-somebody", Namespace: edge}},
		// The operator's, and nothing to do with mail. Left alone.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "edge-kernel-oidc", Namespace: edge, Labels: managed}},
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.RetireMisplacedMailObjectsForTest(ctx); err != nil {
		t.Fatalf("retire: %v", err)
	}
	// Twice: a start after the one that moved everything finds nothing.
	if err := r.RetireMisplacedMailObjectsForTest(ctx); err != nil {
		t.Fatalf("retire, second start: %v", err)
	}

	if got := where(t, c, "ConfigMap", "postfix-kernel-virtual-mailbox-maps"); len(got) != 0 {
		t.Errorf("Postfix's maps are still in %v", got)
	}
	if got := where(t, c, "Secret", "dovecot-app-passwords"); len(got) != 0 {
		t.Errorf("Dovecot's passwd-files are still in %v", got)
	}
	moved := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mail-apppw-seed-" + pinTenant, Namespace: layout.System("mail")}, moved); err != nil {
		t.Fatalf("the seed did not move: %v", err)
	}
	if string(moved.Data["seed"]) != string(seed) {
		t.Error("the seed changed on the way; every password derived from it would have")
	}
	if got := where(t, c, "Secret", "mail-apppw-seed-"+pinTenant); len(got) != 1 {
		t.Errorf("the seed is in %v after the move", got)
	}
	if got := where(t, c, "Secret", "mail-submission-"+pinTenant); len(got) != 1 || got[0] != layout.Namespace(layout.Authentication) {
		t.Errorf("the realm's submission credential is in %v after the move", got)
	}
	for _, name := range []string{"mail-submission-somebody", "edge-kernel-oidc"} {
		if got := where(t, c, "Secret", name); len(got) != 1 || got[0] != edge {
			t.Errorf("%s was not the operator's mail object and is now in %v", name, got)
		}
	}
	// No credential store is left in the namespace that faces the internet.
	left := &corev1.SecretList{}
	if err := c.List(ctx, left, client.InNamespace(edge)); err != nil {
		t.Fatal(err)
	}
	for _, s := range left.Items {
		if s.Labels["app.kubernetes.io/managed-by"] == "gentian-os" && (strings.HasPrefix(s.Name, "mail-") || strings.HasPrefix(s.Name, "dovecot-")) {
			t.Errorf("%s/%s is still in the edge namespace", edge, s.Name)
		}
	}
}

// A tenant's seed written where it used to be is taken over on the tenant's
// own reconcile too, whichever of the two comes first.
func TestMailObjects_AMisplacedSeedIsAdoptedNotReplaced(t *testing.T) {
	t.Parallel()
	r, c, tenant := mailPinCluster(t)
	ctx := context.Background()
	seed := []byte("fedcba9876543210fedcba9876543210")
	if err := c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mail-apppw-seed-" + pinTenant, Namespace: controller.ServicesNamespaceForTest(),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "gentian-os"},
		},
		Data: map[string][]byte{"seed": seed},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncMailAppPasswordsForTest(ctx, tenant); err != nil {
		t.Fatalf("mail passwords: %v", err)
	}
	got := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mail-apppw-seed-" + pinTenant, Namespace: layout.System("mail")}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["seed"]) != string(seed) {
		t.Error("a new seed was made although the tenant had one")
	}
}

// A tenant does not report its mail ready while the map Postfix mounts does
// not name its domain.
func TestMail_NotReadyWhileTheMapPostfixMountsLacksTheDomain(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: layout.System("mail")}},
	).Build()
	r := &controller.TenantReconciler{Client: c, MailServiceMode: "system", KernelDomain: "example.test"}
	ctx := context.Background()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "relay"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Relay Co",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeTransportOnly},
		},
	}

	held, err := r.HoldUntilPostfixAcceptsDomainForTest(ctx, tenant)
	if err != nil || !held {
		t.Fatalf("with no map at all: held=%v err=%v, want held", held, err)
	}
	cond := findCondition(tenant, "MailReady")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "PostfixMapMissing" {
		t.Fatalf("with no map at all the tenant reports %+v", cond)
	}
	if !strings.Contains(cond.Message, layout.System("mail")+"/postfix-kernel-virtual-mailbox-maps") {
		t.Errorf("the message does not name the object and its namespace: %s", cond.Message)
	}

	// The map in the namespace Postfix does not run in changes nothing.
	domain := "relay.example.test"
	if err := c.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "postfix-kernel-virtual-mailbox-maps", Namespace: layout.System("mail-dmz")},
		Data:       map[string]string{"virtual_mailbox_domains": domain + " OK\n"},
	}); err != nil {
		t.Fatal(err)
	}
	if held, _ := r.HoldUntilPostfixAcceptsDomainForTest(ctx, tenant); !held {
		t.Fatal("a map in the mail DMZ, where no Postfix runs, was taken for Postfix's")
	}

	// Provisioned properly, it is ready.
	_ = r.EnsureMailForTest(ctx, tenant)
	cond = findCondition(tenant, "MailReady")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("after provisioning the tenant reports %+v", cond)
	}
}

// The addresses the operator publishes for mail.<kernelDomain> and
// imap.<kernelDomain> are read from the Service in front of the mail edge,
// in the namespace the ApplicationSet syncs that chart into -- and that
// chart is the only one of the three that renders a load balancer.
func TestMailObjects_TheAddressIsReadFromTheEdgeService(t *testing.T) {
	t.Parallel()
	namespaces := mailChartNamespaces(t)
	edgeNS, ok := namespaces["mail-edge"]
	if !ok {
		t.Fatal("the mail ApplicationSet deploys no mail-edge chart")
	}
	if edgeNS != layout.System("mail-dmz") {
		t.Fatalf("the mail edge is synced into %s, not the mail DMZ", edgeNS)
	}
	edge := chartText(t, "mail-edge")
	if !strings.Contains(edge, "name: mail-edge-{{ .Values.env }}\n") || !strings.Contains(edge, "type: LoadBalancer") {
		t.Fatal("the mail-edge chart no longer renders the LoadBalancer Service mail-edge-<env> the operator reads")
	}
	for _, chart := range []string{"postfix", "dovecot"} {
		if namespaces[chart] != layout.System("mail") {
			t.Errorf("%s is synced into %s, not the mail namespace", chart, namespaces[chart])
		}
		if strings.Contains(chartText(t, chart), "type: LoadBalancer") {
			t.Errorf("the %s chart renders a load balancer: nothing in the mail namespace is to be reachable from outside", chart)
		}
	}

	service := func(name, ns, ip string, ports ...int32) *corev1.Service {
		s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		for _, p := range ports {
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Port: p})
		}
		s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
		return s
	}
	addresses := func(objects ...client.Object) (string, string) {
		c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(objects...).Build()
		r := &controller.TenantReconciler{Client: c, KernelDomain: "example.test"}
		return r.KernelMailAddressesForTest(context.Background())
	}

	if smtp, imap := addresses(service("mail-edge-dev", edgeNS, "192.0.2.10", 25, 587, 993)); smtp != "192.0.2.10" || imap != "192.0.2.10" {
		t.Errorf("with the edge's Service present the addresses are %q and %q", smtp, imap)
	}
	// A listener that is switched off has no name pointing at it.
	if smtp, imap := addresses(service("mail-edge-dev", edgeNS, "192.0.2.10", 25, 587)); smtp != "192.0.2.10" || imap != "" {
		t.Errorf("without the IMAPS listener the addresses are %q and %q", smtp, imap)
	}
	// The Services of the layout before the edge, wherever they are, are not
	// what mail arrives on.
	if smtp, imap := addresses(
		service("postfix-dev-smtp", layout.System("mail"), "192.0.2.20", 25),
		service("dovecot-dev-imaps", layout.System("mail"), "192.0.2.21", 993),
		service("postfix-dev-smtp", edgeNS, "192.0.2.22", 25),
		service("mail-edge-dev", layout.System("mail"), "192.0.2.23", 25, 993),
	); smtp != "" || imap != "" {
		t.Errorf("without the edge's Service the operator still publishes %q and %q", smtp, imap)
	}
}

// Where a tenant's people have mailboxes is said on the tenant, for whoever
// removes one of them: the registrar reads it there, and asks about a
// mailbox only where it is set.
func TestTheTenantSaysWhereItsMailboxesAre(t *testing.T) {
	t.Parallel()
	r, _, tenant := mailPinCluster(t)
	_ = r.EnsureMailForTest(context.Background(), tenant)
	if got, want := tenant.Status.MailboxDomain, pinTenant+".example.test"; got != want {
		t.Errorf("status.mailboxDomain = %q, want %q", got, want)
	}
	// A tenant that only sends through the cluster has none.
	tenant.Spec.Mail.Mode = gentianov1alpha1.MailModeTransportOnly
	_ = r.EnsureMailForTest(context.Background(), tenant)
	if got := tenant.Status.MailboxDomain; got != "" {
		t.Errorf("a tenant with no mailboxes on the cluster says %q", got)
	}
}

// A tenant realm's mail login exists before the realm does.
//
// The realm and the Job that write it into Keycloak read it by reference and
// neither waits: declared before the Secret existed, the realm was written
// with authentication on and no password, reported synced, and every
// invitation failed with "no password specified". So it is minted with no
// Keycloak to ask -- this client has none -- and no realm to read users from.
func TestMail_RealmCredentialIsMintedBeforeTheRealm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newTenant := func(name string, mode gentianov1alpha1.MailMode) *gentianov1alpha1.Tenant {
		tn := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		if mode != "" {
			tn.Spec.Mail = &gentianov1alpha1.TenantMail{Mode: mode}
		}
		return tn
	}
	credential := func(c client.Client, tenant string) (*corev1.Secret, error) {
		sec := &corev1.Secret{}
		err := c.Get(ctx, types.NamespacedName{Name: "mail-submission-" + tenant, Namespace: layout.Namespace(layout.Authentication)}, sec)
		return sec, err
	}
	mailNS := func() *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: layout.System("mail")}}
	}

	t.Run("the cluster's own mail: minted, and the same on the next pass", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(mailNS()).Build()
		r := &controller.TenantReconciler{Client: c, MailServiceMode: "system", KernelDomain: "example.org", TenancyMode: "multi"}
		tenant := newTenant("team", "")
		if err := r.EnsureRealmMailCredentialForTest(ctx, tenant); err != nil {
			t.Fatalf("mint: %v", err)
		}
		first, err := credential(c, "team")
		if err != nil {
			t.Fatalf("no credential before the realm: %v", err)
		}
		if got := string(first.Data["smtp_user"]); got != "noreply@team.example.org" {
			t.Errorf("smtp_user = %q, want the login the Composition declares", got)
		}
		if len(first.Data["smtp_password"]) == 0 {
			t.Error("smtp_password is empty")
		}
		// Dovecot learns the same login, or presenting it proves nothing.
		hashes := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: "dovecot-app-passwords", Namespace: layout.System("mail")}, hashes); err != nil {
			t.Fatalf("no passwd-file for the login: %v", err)
		}
		var registered bool
		for _, v := range hashes.Data {
			registered = registered || strings.HasPrefix(string(v), "noreply@team.example.org:")
		}
		if !registered {
			t.Error("the login is in no passwd-file Dovecot reads")
		}
		if err := r.EnsureRealmMailCredentialForTest(ctx, tenant); err != nil {
			t.Fatalf("second pass: %v", err)
		}
		second, _ := credential(c, "team")
		if string(second.Data["smtp_password"]) != string(first.Data["smtp_password"]) {
			t.Error("the password changed between two passes: Keycloak would hold the first and Dovecot the second")
		}
	})

	// Nothing is minted where the mail stage would mint nothing.
	for _, tc := range []struct {
		name   string
		r      controller.TenantReconciler
		tenant *gentianov1alpha1.Tenant
		mailNS bool
	}{
		{"a relay", controller.TenantReconciler{MailServiceMode: "external", KernelDomain: "example.org"}, newTenant("team", ""), true},
		{"no mail function composed", controller.TenantReconciler{MailServiceMode: "system", KernelDomain: "example.org"}, newTenant("team", ""), false},
		{"a tenant that is not registered in the cluster's mail", controller.TenantReconciler{MailServiceMode: "system", KernelDomain: "example.org"}, newTenant("team", gentianov1alpha1.MailModeTransportOnly), true},
		{"the tenant that adopts the kernel realm", controller.TenantReconciler{MailServiceMode: "system", KernelDomain: "example.org", KernelRealm: "kernel"},
			func() *gentianov1alpha1.Tenant {
				tn := newTenant("platform", "")
				tn.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
				return tn
			}(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t))
			if tc.mailNS {
				b = b.WithObjects(mailNS())
			}
			c := b.Build()
			r := tc.r
			r.Client = c
			if err := r.EnsureRealmMailCredentialForTest(ctx, tc.tenant); err != nil {
				t.Fatalf("mint: %v", err)
			}
			if _, err := credential(c, tc.tenant.Name); !apierrors.IsNotFound(err) {
				t.Errorf("a credential was minted (err=%v)", err)
			}
		})
	}
}

// The mail server a realm is declared with is stated only once its password
// exists, and only for a realm that is to send.
//
// The provider behind the Composition does not wait for a Secret it is
// pointed at, and a realm written with a login and no password cannot send.
// On a cluster that relays, the block left undeclared was worse: the provider
// rewrote the mail server the Job had written, without the password.
func TestMail_RealmMailIsStatedOnceItsPasswordExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	idNS := layout.Namespace(layout.Authentication)
	tenant := func(name string, mode gentianov1alpha1.MailMode, signUp bool) *gentianov1alpha1.Tenant {
		tn := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		if mode != "" {
			tn.Spec.Mail = &gentianov1alpha1.TenantMail{Mode: mode}
		}
		if signUp {
			tn.Spec.SignUp = &gentianov1alpha1.TenantSignUp{Enabled: true}
		}
		return tn
	}
	mailNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: layout.System("mail")}}
	relay := func(data map[string]string) *corev1.Secret {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keycloak-smtp-credentials", Namespace: idNS}, Data: map[string][]byte{}}
		for k, v := range data {
			sec.Data[k] = []byte(v)
		}
		return sec
	}
	supplied := map[string]string{
		"smtp_configure": "true", "mail_service_mode": "external",
		"smtp_host": "relay.example.org", "smtp_port": "465", "smtp_ssl": "true", "smtp_starttls": "false",
		"smtp_user": "relay-user", "smtp_password": "relay-secret", "smtp_from": "noreply@example.org",
	}

	t.Run("a relay, supplied: its host and login, and the password by name", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(relay(supplied)).Build()
		r := &controller.TenantReconciler{Client: c, MailServiceMode: "external", KernelDomain: "example.org", KernelRealm: "kernel"}
		got, err := r.TenantRealmMailForTest(ctx, tenant("team", "", false))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]interface{}{
			"host": "relay.example.org", "port": "465", "from": "noreply@example.org", "username": "relay-user",
			"ssl": true, "starttls": false,
			"passwordSecretRef": map[string]interface{}{"name": "keycloak-smtp-credentials", "key": "smtp_password"},
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("realmMail:\n got %v\nwant %v", got, want)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "relay-secret") {
			t.Error("the password is on the XTenant")
		}
	})

	t.Run("a relay not supplied yet: no mail server, and one after it is", func(t *testing.T) {
		notYet := map[string]string{}
		for k, v := range supplied {
			notYet[k] = v
		}
		notYet["smtp_configure"], notYet["smtp_user"], notYet["smtp_password"] = "false", "", ""
		c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(relay(notYet)).Build()
		r := &controller.TenantReconciler{Client: c, MailServiceMode: "external", KernelDomain: "example.org", KernelRealm: "kernel"}
		if got, err := r.TenantRealmMailForTest(ctx, tenant("team", "", false)); err != nil || got != nil {
			t.Fatalf("before the credential: got %v, %v; want none", got, err)
		}
		sec := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: "keycloak-smtp-credentials", Namespace: idNS}, sec); err != nil {
			t.Fatal(err)
		}
		sec.Data = relay(supplied).Data
		if err := c.Update(ctx, sec); err != nil {
			t.Fatal(err)
		}
		if got, err := r.TenantRealmMailForTest(ctx, tenant("team", "", false)); err != nil || got == nil {
			t.Fatalf("after the credential: got %v, %v; want the relay", got, err)
		}
	})

	t.Run("no Secret at all: no mail server", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).Build()
		r := &controller.TenantReconciler{Client: c, MailServiceMode: "external", KernelDomain: "example.org", KernelRealm: "kernel"}
		if got, err := r.TenantRealmMailForTest(ctx, tenant("team", "", false)); err != nil || got != nil {
			t.Fatalf("got %v, %v; want none", got, err)
		}
	})

	// The cluster's own mail: the realm's own login where one is minted, and
	// nothing otherwise -- the kernel realm's login in keycloak-smtp-credentials
	// is never a tenant realm's.
	kernelLogin := relay(map[string]string{
		"smtp_configure": "true", "mail_service_mode": "system", "smtp_host": "mail.example.org", "smtp_port": "587",
		"smtp_user": "gentian-system@example.org", "smtp_password": "kernel-secret", "smtp_from": "noreply@example.org",
	})
	for _, tc := range []struct {
		name   string
		tenant *gentianov1alpha1.Tenant
		mint   bool
		want   string
	}{
		{"own mail, a tenant with mailboxes, minted", tenant("team", "", false), true, "noreply@team.example.org"},
		{"own mail, the same tenant before the login is minted", tenant("team", "", false), false, ""},
		{"own mail, a tenant people sign up to (outbound only)", tenant("team", gentianov1alpha1.MailModeTransportOnly, true), true, "noreply@team.example.org"},
		{"own mail, outbound only and no sign-up: no login is minted", tenant("team", gentianov1alpha1.MailModeTransportOnly, false), true, ""},
		{"own mail, mail disabled", tenant("team", gentianov1alpha1.MailModeDisabled, false), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(mailNS.DeepCopy(), kernelLogin.DeepCopy()).Build()
			r := &controller.TenantReconciler{Client: c, MailServiceMode: "system", KernelDomain: "example.org", KernelRealm: "kernel", TenancyMode: "multi"}
			if tc.mint {
				if err := r.EnsureRealmMailCredentialForTest(ctx, tc.tenant); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.TenantRealmMailForTest(ctx, tc.tenant)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("a mail server is stated: %v", got)
				}
				return
			}
			want := map[string]interface{}{
				"host": "mail.example.org", "port": "587", "from": "noreply@example.org", "username": tc.want,
				"ssl": false, "starttls": true,
				"passwordSecretRef": map[string]interface{}{"name": "mail-submission-team", "key": "smtp_password"},
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("realmMail:\n got %v\nwant %v", got, want)
			}
		})
	}
}

// Every key the operator states for a realm's mail server is one the XTenant
// schema declares: an XRD prunes what it does not, without a word.
func TestMail_RealmMailKeysAreInTheXTenantSchema(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "xrds", "tenant.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var xrd map[string]interface{}
	if err := yaml.Unmarshal(raw, &xrd); err != nil {
		t.Fatal(err)
	}
	versions, _, _ := unstructured.NestedSlice(xrd, "spec", "versions")
	props, found, _ := unstructured.NestedMap(versions[0].(map[string]interface{}),
		"schema", "openAPIV3Schema", "properties", "spec", "properties", "realmMail", "properties")
	if !found {
		t.Fatal("the XTenant schema has no spec.realmMail")
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keycloak-smtp-credentials", Namespace: layout.Namespace(layout.Authentication)},
		Data: map[string][]byte{"smtp_configure": []byte("true"), "smtp_host": []byte("relay.example.org"),
			"smtp_user": []byte("relay-user"), "smtp_password": []byte("x")},
	}).Build()
	r := &controller.TenantReconciler{Client: c, MailServiceMode: "external", KernelDomain: "example.org", KernelRealm: "kernel"}
	got, err := r.TenantRealmMailForTest(context.Background(),
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"}})
	if err != nil || got == nil {
		t.Fatalf("got %v, %v", got, err)
	}
	for k := range got {
		if _, ok := props[k]; !ok {
			t.Errorf("realmMail.%s is stated and the schema does not declare it", k)
		}
	}
	ref, _, _ := unstructured.NestedMap(props, "passwordSecretRef", "properties")
	for k := range got["passwordSecretRef"].(map[string]interface{}) {
		if _, ok := ref[k]; !ok {
			t.Errorf("realmMail.passwordSecretRef.%s is stated and the schema does not declare it", k)
		}
	}
}
