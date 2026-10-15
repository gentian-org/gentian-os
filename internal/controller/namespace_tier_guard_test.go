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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	tierGuardTemplate  = "templates/namespace-tier-guard.yaml"
	tierGuardInstaller = "installer@example.org"
	tierLabel          = "gentianos.io/tier"
)

var bootstrapChart = filepath.Join("..", "..", "kernel", "bootstrap", "chart")

// renderTierGuard renders the guard as the installer does: the layout of
// kernel/namespaces.yaml, and the installer's user name in a values file.
func renderTierGuard(t *testing.T) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is needed to render the bootstrap chart: %v", err)
	}
	layout, err := os.ReadFile(filepath.Join("..", "..", "kernel", "namespaces.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	indented := "namespaces:\n  " + strings.ReplaceAll(string(layout), "\n", "\n  ")
	if err := os.WriteFile(filepath.Join(dir, "namespaces.yaml"), []byte(indented), 0o600); err != nil {
		t.Fatal(err)
	}
	installer := fmt.Sprintf("installer:\n  username: %q\n", tierGuardInstaller)
	if err := os.WriteFile(filepath.Join(dir, "installer.yaml"), []byte(installer), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command("helm", "template", "gentian-bootstrap", bootstrapChart, "-s", tierGuardTemplate,
		"-f", filepath.Join(dir, "namespaces.yaml"), "-f", filepath.Join(dir, "installer.yaml"))
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			policy = &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := yaml.UnmarshalStrict([]byte(doc), policy); err != nil {
				t.Fatal(err)
			}
		case "ValidatingAdmissionPolicyBinding":
			binding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := yaml.UnmarshalStrict([]byte(doc), binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	if policy == nil || binding == nil {
		t.Fatalf("the guard renders no policy and binding:\n%s", out)
	}
	return policy, binding
}

// The operator and provider-kubernetes are named in the guard by the
// ServiceAccounts they run as. A rename on either side would lock the writer
// out of the label it composes, and nothing else would say why.
func TestTheTierGuardNamesTheWritersServiceAccounts(t *testing.T) {
	var values struct {
		NamespaceTier struct {
			OperatorServiceAccount           string `json:"operatorServiceAccount"`
			ProviderKubernetesServiceAccount string `json:"providerKubernetesServiceAccount"`
		} `json:"namespaceTier"`
	}
	raw, err := os.ReadFile(filepath.Join(bootstrapChart, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}

	// The operator's chart, released under the name of its Application.
	out, err := exec.Command("helm", "template", "gentian-os", filepath.Join("..", "..", "charts", "gentian-os"),
		"-s", "templates/serviceaccount.yaml").Output()
	if err != nil {
		t.Fatalf("helm template the operator's ServiceAccount: %v", err)
	}
	var sa corev1.ServiceAccount
	if err := yaml.Unmarshal(out, &sa); err != nil {
		t.Fatal(err)
	}
	if sa.Name != values.NamespaceTier.OperatorServiceAccount {
		t.Errorf("the operator runs as %q and the guard names %q", sa.Name, values.NamespaceTier.OperatorServiceAccount)
	}

	runtimeConfigs, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "providers", "providers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, doc := range strings.Split(string(runtimeConfigs), "\n---\n") {
		var rc struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ServiceAccountTemplate struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"serviceAccountTemplate"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &rc); err != nil {
			t.Fatal(err)
		}
		if rc.Kind == "DeploymentRuntimeConfig" && rc.Metadata.Name == "kubernetes-config" {
			found = true
			if got := rc.Spec.ServiceAccountTemplate.Metadata.Name; got != values.NamespaceTier.ProviderKubernetesServiceAccount {
				t.Errorf("provider-kubernetes runs as %q and the guard names %q", got, values.NamespaceTier.ProviderKubernetesServiceAccount)
			}
		}
	}
	if !found {
		t.Error("crossplane/providers/providers.yaml has no DeploymentRuntimeConfig kubernetes-config")
	}
}

// guardedLabel is one label set the guard keeps, with an example of it and
// those who may write it.
type guardedLabel struct {
	what         string
	key          string
	value, other string
	writers      []string
}

// Against a real API server: a tenant's apps, synced by Argo CD or installed
// by provider-helm under either of its identities, cannot set, change or
// remove a namespace's tier, function or mac-waiver labels; each set's own
// writers can, and no one else; and a write that leaves a set as it was
// passes for anyone.
func TestOnlyThePlatformSetsANamespacesTier(t *testing.T) {
	policy, binding := renderTierGuard(t)

	binDir := "/tmp/envtest-bins/k8s/1.32.0-linux-amd64"
	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		binDir = v
	}
	env := &envtest.Environment{BinaryAssetsDirectory: binDir}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	admin := kubernetes.NewForConfigOrDie(cfg)

	// Everyone below may write namespaces as far as RBAC goes: what is under
	// test is admission alone.
	if _, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "tier-guard-test"},
		Subjects:   []rbacv1.Subject{{Kind: "Group", Name: "tier-guard-test", APIGroup: rbacv1.GroupName}},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin", APIGroup: rbacv1.GroupName},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	as := func(name string, groups ...string) *kubernetes.Clientset {
		t.Helper()
		u, err := env.AddUser(envtest.User{Name: name, Groups: append(groups, "tier-guard-test")}, rest.CopyConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		return kubernetes.NewForConfigOrDie(u.Config())
	}
	who := map[string]*kubernetes.Clientset{
		"the installer":       as(tierGuardInstaller),
		"provider-kubernetes": as("system:serviceaccount:kernel-provisioning:provider-kubernetes"),
		"the operator":        as("system:serviceaccount:kernel-control:gentian-os"),
		"system:masters":      as("break-glass", "system:masters"),
		// Never a writer of any set.
		"Argo CD's application controller":  as("system:serviceaccount:kernel-gitops:argocd-application-controller"),
		"provider-helm":                     as("system:serviceaccount:kernel-provisioning:provider-helm"),
		"provider-helm for a tenant's apps": as("system:serviceaccount:kernel-provisioning:" + tenantAppsHelmServiceAccount),
		"a tenant's own ServiceAccount":     as("system:serviceaccount:tenant-acme:default"),
	}
	sets := []guardedLabel{
		{what: "tier", key: tierLabel, value: "kernel", other: "system",
			writers: []string{"the installer", "provider-kubernetes", "the operator", "system:masters"}},
		{what: "function", key: "gentianos.io/function", value: "secrets", other: "postgresql",
			writers: []string{"the installer", "provider-kubernetes", "system:masters"}},
		{what: "mac waiver", key: "mac-waiver.gentianos.io/gentian-require-non-root", value: "approved", other: "denied",
			writers: []string{"the operator", "system:masters"}},
	}

	if _, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		t.Fatalf("the API server refuses the policy: %v", err)
	}
	if _, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatalf("the API server refuses the binding: %v", err)
	}

	seq := 0
	name := func() string { seq++; return fmt.Sprintf("tenant-guard-%d", seq) }
	create := func(c *kubernetes.Clientset, ns string, labels map[string]string) error {
		_, err := c.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels},
		}, metav1.CreateOptions{})
		return err
	}
	relabel := func(c *kubernetes.Clientset, ns string, edit func(map[string]string)) error {
		got, err := c.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if got.Labels == nil {
			got.Labels = map[string]string{}
		}
		edit(got.Labels)
		_, err = c.CoreV1().Namespaces().Update(ctx, got, metav1.UpdateOptions{})
		return err
	}

	// The policy takes effect a moment after it is written.
	deadline := time.Now().Add(time.Minute)
	for {
		err := create(who["a tenant's own ServiceAccount"], name(), map[string]string{tierLabel: "kernel"})
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the guard never refused a namespace created into the kernel tier")
		}
		time.Sleep(250 * time.Millisecond)
	}

	refused := func(who, what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s could %s", who, what)
		} else if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
			t.Errorf("%s, %s: %v", who, what, err)
		} else if !strings.Contains(err.Error(), "set by the platform alone") {
			t.Errorf("%s, %s, refused with another message: %v", who, what, err)
		}
	}
	allowed := func(who, what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s cannot %s: %v", who, what, err)
		}
	}

	for _, set := range sets {
		isWriter := map[string]bool{}
		for _, w := range set.writers {
			isWriter[w] = true
		}
		// A namespace carrying the set, as its first writer labels one.
		labelled := func() string {
			t.Helper()
			ns := name()
			if err := create(who[set.writers[0]], ns, map[string]string{set.key: set.value, tierLabel: "tenant"}); err != nil {
				t.Fatalf("%s cannot label a namespace with its %s: %v", set.writers[0], set.what, err)
			}
			return ns
		}
		for id, c := range who {
			if id == set.writers[0] {
				continue
			}
			if isWriter[id] {
				ns := name()
				allowed(id, "create a namespace with a "+set.what, create(c, ns, map[string]string{set.key: set.value}))
				allowed(id, "change a namespace's "+set.what, relabel(c, ns, func(l map[string]string) { l[set.key] = set.other }))
				allowed(id, "remove a namespace's "+set.what, relabel(c, ns, func(l map[string]string) { delete(l, set.key) }))
				allowed(id, "give an unlabelled namespace a "+set.what, relabel(c, ns, func(l map[string]string) { l[set.key] = set.value }))
				ns = labelled()
				allowed(id, "change a "+set.what+" another writer set", relabel(c, ns, func(l map[string]string) { l[set.key] = set.other }))
				continue
			}
			refused(id, "create a namespace with a "+set.what, create(c, name(), map[string]string{set.key: set.value}))

			ns := name()
			allowed(id, "create a namespace with no "+set.what, create(c, ns, map[string]string{"app": "x"}))
			refused(id, "give an unlabelled namespace a "+set.what, relabel(c, ns, func(l map[string]string) { l[set.key] = set.value }))

			ns = labelled()
			refused(id, "change a namespace's "+set.what, relabel(c, ns, func(l map[string]string) { l[set.key] = set.other }))
			refused(id, "remove a namespace's "+set.what, relabel(c, ns, func(l map[string]string) { delete(l, set.key) }))
			refused(id, "set a namespace's "+set.what+" to empty", relabel(c, ns, func(l map[string]string) { l[set.key] = "" }))
			allowed(id, "write a namespace with its "+set.what+" as it was", relabel(c, ns, func(l map[string]string) { l[set.key] = set.value }))
			allowed(id, "change another label of a namespace with a "+set.what, relabel(c, ns, func(l map[string]string) { l["app"] = "y" }))
			if strings.HasPrefix(set.key, "mac-waiver.gentianos.io/") {
				refused(id, "add a waiver beside the granted one", relabel(c, ns, func(l map[string]string) {
					l["mac-waiver.gentianos.io/gentian-disallow-host-path"] = "approved"
				}))
			}
		}
	}
}
