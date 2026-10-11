/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

func mailboxProfile(tokenSignIn, oidcClient bool) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = "webmail"
	s := &gentianov1alpha1.ServiceRequirements{
		Mail: &gentianov1alpha1.MailRequirement{IMAP: &gentianov1alpha1.IMAPRequirement{TokenSignIn: tokenSignIn}},
	}
	if oidcClient {
		s.Identity = &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "webmail"}}
	}
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: s}
	return p
}

// Token sign-in is said, never implied: reading mail (imap) and sending it
// (smtp) do not declare it, and it is nothing without the sign-in client the
// scope is given to.
func TestMailboxTokenSignInIsDeclaredExplicitly(t *testing.T) {
	if !declaresMailboxTokenSignIn(mailboxProfile(true, true)) {
		t.Fatal("a profile that declares mail.imap.tokenSignIn beside its sign-in client is not counted")
	}
	if declaresMailboxTokenSignIn(mailboxProfile(false, true)) {
		t.Fatal("mail.imap alone was read as token sign-in: an app that reads mail with app passwords would be given the scope")
	}
	if declaresMailboxTokenSignIn(mailboxProfile(true, false)) {
		t.Fatal("declared without a sign-in client, and counted: there is no client to give the scope to")
	}
	smtpOnly := mailboxProfile(false, true)
	smtpOnly.Spec.Requires.Services.Mail = &gentianov1alpha1.MailRequirement{SMTP: &gentianov1alpha1.SMTPRequirement{}}
	if declaresMailboxTokenSignIn(smtpOnly) {
		t.Fatal("an app that only sends mail was counted")
	}
	if declaresMailboxTokenSignIn(&gentianov1alpha1.ComponentProfile{}) {
		t.Fatal("a profile with no requirements was counted")
	}
}

// Served where the scope exists: a cluster with its own mail server, a tenant
// registered in it, a realm of the tenant's own.
func TestMailboxTokenSignInIsServedWhereTheTenantHasMailboxes(t *testing.T) {
	withMode := func(mode gentianov1alpha1.MailMode) *gentianov1alpha1.Tenant {
		tn := acmeTenantFixture()
		if mode != "" {
			tn.Spec.Mail = &gentianov1alpha1.TenantMail{Mode: mode}
		}
		return tn
	}
	for _, c := range []struct {
		name    string
		tenant  *gentianov1alpha1.Tenant
		cluster string
		want    bool
	}{
		{"own mail server, tenant does not say", withMode(""), "system", true},
		{"own mail server, tenant says selfhosted", withMode(gentianov1alpha1.MailModeSelfhosted), "system", true},
		{"own mail server, tenant sends only", withMode(gentianov1alpha1.MailModeTransportOnly), "system", false},
		{"own mail server, tenant's mail is elsewhere", withMode(gentianov1alpha1.MailModeExternal), "system", false},
		{"own mail server, tenant has no mail", withMode(gentianov1alpha1.MailModeDisabled), "system", false},
		{"mail at a provider", withMode(""), "external", false},
		{"no mail", withMode(""), "none", false},
		{"a cluster that cannot answer yet", withMode(""), "", false},
		{"the name the mode had before", withMode(""), "kernel", false},
		{"the tenant that adopts the kernel realm", platformTenantFixture(), "system", false},
	} {
		if got := tenantHasTokenMailboxes(c.tenant, c.cluster, "kernel"); got != c.want {
			t.Errorf("%s: served = %v, want %v", c.name, got, c.want)
		}
	}
}

// The reconciler puts the fact on the App claim for a declaring app on a
// cluster that serves it, and takes it off again when the profile stops
// declaring -- which is what takes the scope off the app's client.
func TestTheClaimCarriesMailboxTokenSignIn(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	tenant := acmeTenantFixture()

	clusterConfig := func(mode string) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{}
		cm.Name, cm.Namespace = clusterConfigName, clusterConfigNamespace
		cm.Data = map[string]string{clusterConfigMailModeKey: mode}
		return cm
	}
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "webmail", tenantNamespaceName(tenant), "uid-webmail"
	comp.Spec.ProfileRef.Name = "webmail"
	zone := edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}

	claimed := func(r *ComponentReconciler) (bool, bool) {
		t.Helper()
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(appClaimGVK)
		if err := r.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim); err != nil {
			t.Fatal(err)
		}
		v, has, _ := unstructured.NestedBool(claim.Object, "spec", "mailboxTokenSignIn")
		return v, has
	}
	write := func(r *ComponentReconciler, profile *gentianov1alpha1.ComponentProfile) {
		t.Helper()
		if _, _, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", r.mailboxTokenSignIn(ctx, tenant, profile), nil); err != nil {
			t.Fatal(err)
		}
	}

	system := &ComponentReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterConfig("system")).Build(),
		Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel",
	}
	write(system, mailboxProfile(true, true))
	if v, has := claimed(system); !has || !v {
		t.Fatalf("a declaring app on a cluster with its own mail server: claim says %v (present %v)", v, has)
	}
	// The profile stops declaring: the field goes, it is not left at true.
	write(system, mailboxProfile(false, true))
	if _, has := claimed(system); has {
		t.Fatal("the profile stopped declaring and the claim still carries mailboxTokenSignIn")
	}
	// And comes back.
	write(system, mailboxProfile(true, true))
	if v, _ := claimed(system); !v {
		t.Fatal("declared again, and the claim does not say so")
	}

	// A cluster whose mail is at a provider accepts the declaration and
	// grants nothing.
	external := &ComponentReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterConfig("external")).Build(),
		Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel",
	}
	write(external, mailboxProfile(true, true))
	if _, has := claimed(external); has {
		t.Fatal("a cluster without its own mail server put mailboxTokenSignIn on the claim")
	}
}

// renderedObjects reads the objects a render fixture expects.
func renderedObjects(t *testing.T, fixture string) []map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "tests", "unit", "render", fixture, "expected.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]interface{}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		obj := map[string]interface{}{}
		if err := dec.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("%s: %v", fixture, err)
		}
		if len(obj) > 0 {
			out = append(out, obj)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s renders nothing", fixture)
	}
	return out
}

func ofKind(objs []map[string]interface{}, kind string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, o := range objs {
		if o["kind"] == kind {
			out = append(out, o)
		}
	}
	return out
}

// What a tenant's realm is given on a cluster with its own mail server: the
// scope, and on it the one mapper that names the mail server in an access
// token's audience -- pinned key by key, because each key is part of the
// grant. Nothing else in the realm names gentian-dovecot as an audience.
func TestTheMailboxScopeAsRendered(t *testing.T) {
	objs := renderedObjects(t, "tenant-default")

	var scope map[string]interface{}
	for _, o := range ofKind(objs, "ClientScope") {
		if name, _, _ := unstructured.NestedString(o, "spec", "forProvider", "name"); name == mailboxScopeName {
			scope = o
		}
	}
	if scope == nil {
		t.Fatal("a tenant on a cluster with its own mail server renders no client scope \"mailbox\"")
	}
	if in, _, _ := unstructured.NestedBool(scope, "spec", "forProvider", "includeInTokenScope"); !in {
		t.Fatal("the scope is not included in the token's scope claim, which is what the mail server reads")
	}

	var audiences []map[string]interface{}
	for _, o := range ofKind(objs, "ProtocolMapper") {
		cfg, _, _ := unstructured.NestedStringMap(o, "spec", "forProvider", "config")
		for k, v := range cfg {
			if strings.HasPrefix(k, "included.") && v == dovecotOIDCClientID {
				audiences = append(audiences, o)
			}
		}
	}
	if len(audiences) != 1 {
		t.Fatalf("%d mappers put %s in an audience, want exactly the one on the mailbox scope", len(audiences), dovecotOIDCClientID)
	}
	mapper := audiences[0]
	cfg, _, _ := unstructured.NestedStringMap(mapper, "spec", "forProvider", "config")
	want := map[string]string{
		// A client audience: Keycloak leaves it out when gentian-dovecot is
		// disabled or gone. A custom audience would name it regardless.
		"included.client.audience":  dovecotOIDCClientID,
		"access.token.claim":        "true",
		"id.token.claim":            "false",
		"userinfo.token.claim":      "false",
		"introspection.token.claim": "true",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("the audience mapper's config is\n  %v\nwant\n  %v", cfg, want)
	}
	if pm, _, _ := unstructured.NestedString(mapper, "spec", "forProvider", "protocolMapper"); pm != "oidc-audience-mapper" {
		t.Fatalf("the mapper is a %q", pm)
	}
	// On the scope, not on a client: a mapper on a client is in every token
	// of that client.
	if id, _, _ := unstructured.NestedString(mapper, "spec", "forProvider", "clientScopeId"); id == "" {
		t.Fatal("the audience mapper is not on a client scope")
	}
	for _, f := range []string{"clientId", "clientIdRef", "clientIdSelector"} {
		if _, has, _ := unstructured.NestedFieldNoCopy(mapper, "spec", "forProvider", f); has {
			t.Fatalf("the audience mapper is on a client (%s)", f)
		}
	}

	// The tenant that adopts the kernel realm composes none of it.
	for _, o := range renderedObjects(t, "tenant-platform") {
		if name, _, _ := unstructured.NestedString(o, "spec", "forProvider", "name"); name == mailboxScopeName || name == "mailbox-audience" {
			t.Fatalf("the platform tenant renders %v %q in a realm that is not its own", o["kind"], name)
		}
	}
}

// Which app's client is given the scope: the one whose profile declares it,
// on a claim that says the tenant is served. An app that stops declaring
// keeps the object and loses "mailbox"; one that never declared has no such
// object at all.
func TestTheMailboxScopeIsGivenToDeclaringAppsOnly(t *testing.T) {
	keycloaksOwn := []interface{}{"address", "microprofile-jwt", "offline_access", "organization", "phone"}
	for _, c := range []struct {
		fixture string
		want    []interface{}
	}{
		{"app-mailbox-token", append(append([]interface{}{}, keycloaksOwn...), mailboxScopeName)},
		{"app-mailbox-token-withdrawn", keycloaksOwn},
		{"app-mailbox-token-not-served", nil},
		{"app-mailbox-token-undeclared", nil},
		{"app-default", nil},
		{"app-model-gateway", nil},
	} {
		objs := renderedObjects(t, c.fixture)
		lists := ofKind(objs, "ClientOptionalScopes")
		if c.want == nil {
			if len(lists) != 0 {
				t.Errorf("%s: renders optional scopes for a client that was never given the mailbox scope", c.fixture)
			}
		} else {
			if len(lists) != 1 {
				t.Errorf("%s: %d ClientOptionalScopes, want 1", c.fixture, len(lists))
				continue
			}
			got, _, _ := unstructured.NestedSlice(lists[0], "spec", "forProvider", "optionalScopes")
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: optional scopes %v, want %v", c.fixture, got, c.want)
			}
			// Orphan: see the Composition. Delete would have an uninstall
			// detach scopes from a client that is already gone.
			if p, _, _ := unstructured.NestedString(lists[0], "spec", "deletionPolicy"); p != "Orphan" {
				t.Errorf("%s: deletionPolicy %q", c.fixture, p)
			}
		}
		// No app is given the audience directly: a mapper on the app's own
		// client would be in every token it gets.
		for _, m := range ofKind(objs, "ProtocolMapper") {
			cfg, _, _ := unstructured.NestedStringMap(m, "spec", "forProvider", "config")
			for _, v := range cfg {
				if v == dovecotOIDCClientID {
					t.Errorf("%s: a mapper of the app names %s", c.fixture, dovecotOIDCClientID)
				}
			}
		}
	}
}

// The mail server's check of a realm, whole. The scope line is the one thing
// added; the user is still the token's email claim, and the answer still has
// to say active.
func TestDovecotRequiresTheMailboxScope(t *testing.T) {
	conf, ext := dovecotRealmAuthFiles("acme", "https://id.k.example/", "s3cret")
	wantExt := `introspection_mode = post
introspection_url = https://id.k.example/realms/acme/protocol/openid-connect/token/introspect
client_id = gentian-dovecot
client_secret = s3cret
username_attribute = email
active_attribute = active
active_value = true
scope = mailbox
`
	if ext != wantExt {
		t.Fatalf("the realm's oauth2 settings are\n%s\nwant\n%s", ext, wantExt)
	}
	if !strings.Contains(conf, "driver = oauth2") || !strings.Contains(conf, "mechanisms = xoauth2") ||
		!strings.Contains(conf, "result_failure = continue") {
		t.Fatalf("the realm's passdb changed:\n%s", conf)
	}
}

// One name in three places: what Dovecot requires, what the tenant's realm
// is given, and what the app's client is given.
func TestMailboxScopeHasOneName(t *testing.T) {
	for file, must := range map[string]string{
		"tenant-default.yaml": "                name: " + mailboxScopeName + "\n",
		"app-default.yaml":    "                  - " + mailboxScopeName + "\n",
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "compositions", file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), must) {
			t.Errorf("%s does not name the scope %q that the mail server requires", file, mailboxScopeName)
		}
	}
}

// gentian-dovecot is told to check the audience like any other client.
//
// Keycloak has a client attribute that switches the check off for one client,
// and with it the mail server would accept every token of the realm again,
// whichever app it was issued to. Nothing the platform ships sets it: not a
// Composition, not a chart, not a script, not the operator -- and not the
// script that makes the client in the kernel realm, or anything rendered.
func TestNothingSwitchesTheIntrospectionAudienceCheckOff(t *testing.T) {
	attribute := strings.Join([]string{"allow", "token", "introspection", "without", "audience", "check"}, ".")

	script := buildOIDCPackScript("kernel", dovecotOIDCClientID, oidc.Pack{ServiceClient: true}, nil, nil, "", "")
	if strings.Contains(script, attribute) {
		t.Fatal("the script that makes gentian-dovecot in the kernel realm sets " + attribute)
	}

	root := filepath.Join("..", "..")
	for _, dir := range []string{"crossplane", "charts", "kernel", "scripts", "internal", "cmd", "api", "config", "install"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, "_test.go") || !d.Type().IsRegular() {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(raw, []byte(attribute)) {
				t.Errorf("%s sets or names %s", strings.TrimPrefix(path, root+string(filepath.Separator)), attribute)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
