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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// The two guards an add-on's privileges are held by, against a real API
// server: what an export a component starts may be, and what the operator
// may write of the cluster's API when it installs an add-on's types.

const guardOperator = "system:serviceaccount:kernel-control:gentian-os"

// renderOperatorGuard renders one admission guard of the operator's chart as
// it is released: gentian-os, into kernel-control.
func renderOperatorGuard(t *testing.T, template string) []client.Object {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is needed to render the operator's chart: %v", err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command("helm", "template", "gentian-os", filepath.Join("..", "..", "charts", "gentian-os"),
		"--namespace", "kernel-control", "-s", template)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var objs []client.Object
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			p := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := yaml.UnmarshalStrict([]byte(doc), p); err != nil {
				t.Fatal(err)
			}
			objs = append(objs, p)
		case "ValidatingAdmissionPolicyBinding":
			b := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := yaml.UnmarshalStrict([]byte(doc), b); err != nil {
				t.Fatal(err)
			}
			objs = append(objs, b)
		}
	}
	if len(objs) != 2 {
		t.Fatalf("%s renders %d objects, want a policy and its binding:\n%s", template, len(objs), out)
	}
	return objs
}

// guardEnv starts an API server with the platform's definitions and the
// guards, and returns a client for whoever is named. Everyone may do
// anything as far as RBAC goes: what is under test is admission alone.
func guardEnv(t *testing.T, templates ...string) func(user string) client.Client {
	t.Helper()
	binDir := "/tmp/envtest-bins/k8s/1.32.0-linux-amd64"
	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		binDir = v
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: binDir,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "gentian-os", "crds")},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{scheme.AddToScheme, gentianov1alpha1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "addon-guard-test"},
		Subjects:   []rbacv1.Subject{{Kind: "Group", Name: "addon-guard-test", APIGroup: rbacv1.GroupName}},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin", APIGroup: rbacv1.GroupName},
	}); err != nil {
		t.Fatal(err)
	}
	for _, template := range templates {
		for _, obj := range renderOperatorGuard(t, template) {
			if err := admin.Create(ctx, obj); err != nil {
				t.Fatalf("the API server refuses %s: %v", template, err)
			}
		}
	}
	return func(user string) client.Client {
		t.Helper()
		u, err := env.AddUser(envtest.User{Name: user, Groups: []string{"addon-guard-test"}}, rest.CopyConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(u.Config(), client.Options{Scheme: s})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
}

// untilRefused retries a write the guard should refuse until it does: a
// policy takes effect a moment after it is written.
func untilRefused(t *testing.T, what string, write func(n int) error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for n := 0; ; n++ {
		err := write(n)
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			return
		}
		if err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the guard never refused %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// An export a component starts goes to the cluster's storage, the tenant's
// policy or a store the tenant declared, encrypted to the cluster's recovery
// keys; the operator's, for the console and the platform's own schedules,
// are not held to that.
func TestAnExportAComponentStartsStaysWhereTheTenantDeclared(t *testing.T) {
	as := guardEnv(t, "templates/tenant-export-guard.yaml")
	ctx := context.Background()
	addon := as("system:serviceaccount:tenant-platform:backup")
	operator := as(guardOperator)
	ns := "tenant-acme"
	if err := as("admin").Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	export := func(name string, d *gentianov1alpha1.ExportDestination, e *gentianov1alpha1.ExportEncryption) *gentianov1alpha1.TenantExport {
		return &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       gentianov1alpha1.TenantExportSpec{Destination: d, Encryption: e},
		}
	}
	custom := &gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationCustom, Endpoint: "https://s3.elsewhere.example", Bucket: "taken"}
	untilRefused(t, "an export to an endpoint of the component's", func(n int) error {
		return addon.Create(ctx, export(fmt.Sprintf("probe-%d", n), custom, nil))
	})

	ownKey := &gentianov1alpha1.ExportEncryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{"age1ownkeyofthecomponent"}}
	passphrase := &gentianov1alpha1.ExportEncryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "some-secret"}}
	cases := []struct {
		name     string
		obj      *gentianov1alpha1.TenantExport
		admitted bool
	}{
		{"the tenant's policy", export("policy", nil, nil), true},
		{"the cluster's storage", export("platform", &gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationPlatform}, nil), true},
		{"a store the tenant declared", export("store", &gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationSource, Source: "acme.offsite"}, nil), true},
		{"the cluster's recovery keys, said", export("recipient", nil, &gentianov1alpha1.ExportEncryption{Mode: gentianov1alpha1.ExportEncryptionRecipient}), true},
		{"an endpoint of its own", export("custom", custom, nil), false},
		{"a key of its own", export("own-key", nil, ownKey), false},
		{"a passphrase", export("passphrase", nil, passphrase), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := addon.Create(ctx, tc.obj.DeepCopy())
			switch {
			case tc.admitted && err != nil:
				t.Errorf("refused: %v", err)
			case !tc.admitted && err == nil:
				t.Error("admitted")
			case !tc.admitted && !strings.Contains(err.Error(), "may export only"):
				t.Errorf("refused, but not by the guard: %v", err)
			}
		})
	}
	// The operator is not held: the console's own export names a key.
	if err := operator.Create(ctx, export("operator-own-key", nil, ownKey)); err != nil {
		t.Errorf("the operator's export to a person's key was refused: %v", err)
	}
	// Nor is a person.
	if err := as("break-glass").Create(ctx, export("person-custom", custom, nil)); err != nil {
		t.Errorf("a person's export was refused: %v", err)
	}
}

// The operator writes only an add-on's definitions, labeled with its
// profile, outside Kubernetes' and the platform's groups, without a
// conversion webhook, and changes only those it installed for the same
// profile. Every other writer is not looked at.
func TestTheOperatorWritesOnlyAnAddOnsOwnTypes(t *testing.T) {
	as := guardEnv(t, "templates/api-extensions-guard.yaml")
	ctx := context.Background()
	operator := as(guardOperator)
	admin := as("admin")

	crd := func(group, plural, profile string) *apiextensionsv1.CustomResourceDefinition {
		labels := map[string]string{}
		if profile != "" {
			labels[security.APIExtensionLabel] = profile
		}
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group, Labels: labels},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: group,
				Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Kind: strings.ToUpper(plural[:1]) + plural[1:], ListKind: strings.ToUpper(plural[:1]) + plural[1:] + "List"},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name: "v1", Served: true, Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object", XPreserveUnknownFields: ptr(true),
					}},
				}},
			},
		}
	}
	untilRefused(t, "an unlabeled definition", func(n int) error {
		return operator.Create(ctx, crd("probe.example.org", "probes"+strings.Repeat("x", n%5), ""))
	})

	refusedCreates := map[string]*apiextensionsv1.CustomResourceDefinition{
		"without a profile":     crd("backup.example.org", "unlabeled", ""),
		"in the platform group": crd("gentianos.io", "schedules", "backup"),
		"in a platform domain":  crd("ext.gentianos.io", "schedules", "backup"),
		"in a Kubernetes group": crd("x.k8s.io", "schedules", "backup"),
	}
	webhook := crd("backup.example.org", "converted", "backup")
	webhook.Spec.Versions = append(webhook.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
		Name: "v2", Served: true, Storage: false, Schema: webhook.Spec.Versions[0].Schema,
	})
	webhook.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
		Strategy: apiextensionsv1.WebhookConverter,
		Webhook: &apiextensionsv1.WebhookConversion{
			ConversionReviewVersions: []string{"v1"},
			ClientConfig:             &apiextensionsv1.WebhookClientConfig{URL: ptr("https://addon.example/convert")},
		},
	}
	refusedCreates["with a conversion webhook"] = webhook
	for name, obj := range refusedCreates {
		if err := operator.Create(ctx, obj); err == nil {
			t.Errorf("the operator created a definition %s", name)
		} else if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
			t.Errorf("a definition %s: %v", name, err)
		}
	}

	// Its own: created, and changed.
	own := crd("backup.example.org", "schedules", "backup")
	if err := operator.Create(ctx, own); err != nil {
		t.Fatalf("the operator could not install an add-on's type: %v", err)
	}
	// Changed on what is read back: the API server marks a new definition
	// established, which moves its version under a write.
	relabel := func(c client.Client, obj *apiextensionsv1.CustomResourceDefinition, edit func(*apiextensionsv1.CustomResourceDefinition)) error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				return err
			}
			edit(obj)
			return c.Update(ctx, obj)
		})
	}
	if err := relabel(operator, own, func(o *apiextensionsv1.CustomResourceDefinition) {
		o.Spec.Names.ShortNames = []string{"bs"}
	}); err != nil {
		t.Errorf("the operator could not change a type it installed: %v", err)
	}
	// Another profile's, or one nobody labeled, is not changed by it.
	other := crd("other.example.org", "things", "other-addon")
	if err := admin.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := relabel(operator, other, func(o *apiextensionsv1.CustomResourceDefinition) {
		o.Labels[security.APIExtensionLabel] = "backup"
	}); err == nil {
		t.Error("the operator took another profile's type for its own")
	}
	foreign := crd("foreign.example.org", "things", "")
	if err := admin.Create(ctx, foreign); err != nil {
		t.Errorf("a writer other than the operator was held: %v", err)
	}
	if err := relabel(operator, foreign, func(o *apiextensionsv1.CustomResourceDefinition) {
		o.Labels = map[string]string{security.APIExtensionLabel: "backup"}
	}); err == nil {
		t.Error("the operator adopted a type nobody installed for a profile")
	}
}

func ptrTo[T any](v T) *T { return &v }
