/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// rbacDocs reads the objects of one kind from a YAML stream.
func rbacDocs[T any](t *testing.T, raw []byte, kind string) []*T {
	t.Helper()
	var out []*T
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind != kind {
			continue
		}
		obj := new(T)
		if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
			t.Fatal(err)
		}
		out = append(out, obj)
	}
	return out
}

// Against a real API server, as provider-helm acts through the ProviderConfig
// tenant-apps: impersonating provider-helm-tenant-apps, bound in one tenant
// namespace by the operator under the operator's own role. What the tenant
// charts of the catalogue render goes through, their own Roles included;
// anything at cluster scope, in another namespace, or of the platform's kinds
// in the tenant's namespace is refused.
func TestTheTenantAppsAccountCanInstallTenantChartsAndNothingElse(t *testing.T) {
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

	// The provider manifests as B-05 applies them.
	raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "providers", "provider-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range rbacDocs[rbacv1.ClusterRole](t, raw, "ClusterRole") {
		if _, err := admin.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// The operator's role as its chart ships it, bound to the operator.
	out, err := exec.Command("helm", "template", "gentian-os", filepath.Join("..", "..", "charts", "gentian-os"),
		"-s", "templates/clusterrole.yaml").Output()
	if err != nil {
		t.Fatalf("helm template the operator's ClusterRole: %v", err)
	}
	operatorRole := rbacDocs[rbacv1.ClusterRole](t, out, "ClusterRole")[0]
	if _, err := admin.RbacV1().ClusterRoles().Create(ctx, operatorRole, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "operator", APIGroup: rbacv1.GroupName}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: operatorRole.Name},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"tenant-acme", "tenant-other", layout.Kernel(layout.Secrets), layout.Kernel(layout.Provisioning)} {
		if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// The operator binds the account in the tenant namespace, and can bind
	// nothing wider with the same permission.
	opCfg := rest.CopyConfig(cfg)
	opCfg.Impersonate = rest.ImpersonationConfig{UserName: "operator"}
	operator := kubernetes.NewForConfigOrDie(opCfg)
	if _, err := operator.RbacV1().RoleBindings("tenant-acme").Create(ctx, desiredTenantAppsBinding("tenant-acme"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("the operator cannot bind the account: %v", err)
	}
	wider := desiredTenantAppsBinding("tenant-acme")
	wider.Name = "wider"
	wider.RoleRef.Name = "cluster-admin"
	if _, err := operator.RbacV1().RoleBindings("tenant-acme").Create(ctx, wider, metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("the operator could bind cluster-admin in a tenant namespace: %v", err)
	}

	// provider-helm through tenant-apps: its own credential, acting as the
	// account (what B-05's kubeconfig says).
	helmCfg := rest.CopyConfig(cfg)
	helmCfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + layout.Kernel(layout.Provisioning) + ":" + tenantAppsHelmServiceAccount}
	helm := kubernetes.NewForConfigOrDie(helmCfg)

	allowed := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("a tenant chart cannot %s: %v", what, err)
		}
	}
	refused := func(what string, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) {
			t.Errorf("a tenant chart could %s: %v", what, err)
		}
	}
	om := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }
	labels := map[string]string{"app": "x"}
	pod := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "x"}}, RestartPolicy: corev1.RestartPolicyNever},
	}
	in := "tenant-acme"

	// What the catalogue's tenant charts render.
	_, err = helm.CoreV1().ConfigMaps(in).Create(ctx, &corev1.ConfigMap{ObjectMeta: om("c")}, metav1.CreateOptions{})
	allowed("create a ConfigMap", err)
	_, err = helm.CoreV1().Secrets(in).Create(ctx, &corev1.Secret{ObjectMeta: om("app-release-signingkey")}, metav1.CreateOptions{})
	allowed("create a Secret", err)
	_, err = helm.CoreV1().Secrets(in).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
	allowed("read Helm's release records", err)
	_, err = helm.CoreV1().Services(in).Create(ctx, &corev1.Service{ObjectMeta: om("s"), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}, metav1.CreateOptions{})
	allowed("create a Service", err)
	_, err = helm.CoreV1().ServiceAccounts(in).Create(ctx, &corev1.ServiceAccount{ObjectMeta: om("sa")}, metav1.CreateOptions{})
	allowed("create a ServiceAccount", err)
	_, err = helm.CoreV1().PersistentVolumeClaims(in).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: om("p"), Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
	}}, metav1.CreateOptions{})
	allowed("create a PersistentVolumeClaim", err)
	_, err = helm.AppsV1().Deployments(in).Create(ctx, &appsv1.Deployment{ObjectMeta: om("d"), Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: withRestart(pod, corev1.RestartPolicyAlways),
	}}, metav1.CreateOptions{})
	allowed("create a Deployment", err)
	_, err = helm.AppsV1().StatefulSets(in).Create(ctx, &appsv1.StatefulSet{ObjectMeta: om("st"), Spec: appsv1.StatefulSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: withRestart(pod, corev1.RestartPolicyAlways),
	}}, metav1.CreateOptions{})
	allowed("create a StatefulSet", err)
	_, err = helm.BatchV1().Jobs(in).Create(ctx, &batchv1.Job{ObjectMeta: om("j"), Spec: batchv1.JobSpec{Template: pod}}, metav1.CreateOptions{})
	allowed("create a Job", err)
	pathType := networkingv1.PathTypePrefix
	_, err = helm.NetworkingV1().Ingresses(in).Create(ctx, &networkingv1.Ingress{ObjectMeta: om("i"), Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
		Host: "matrix.acme.example.org",
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
			Path: "/", PathType: &pathType,
			Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "s", Port: networkingv1.ServiceBackendPort{Number: 80}}},
		}}}},
	}}}}, metav1.CreateOptions{})
	allowed("create an Ingress", err)
	_, err = helm.CoreV1().Pods(in).List(ctx, metav1.ListOptions{})
	allowed("watch its pods come up", err)
	_, err = helm.AppsV1().ReplicaSets(in).List(ctx, metav1.ListOptions{})
	allowed("watch its replica sets", err)
	// Synapse's signing-key Job role, and Activepieces' role.
	_, err = helm.RbacV1().Roles(in).Create(ctx, &rbacv1.Role{ObjectMeta: om("signingkey"), Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"app-release-signingkey"}, Verbs: []string{"get", "update", "patch"},
	}}}, metav1.CreateOptions{})
	allowed("create its signing-key Role", err)
	_, err = helm.RbacV1().Roles(in).Create(ctx, &rbacv1.Role{ObjectMeta: om("reader"), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}},
	}}, metav1.CreateOptions{})
	allowed("create its reader Role", err)
	_, err = helm.RbacV1().RoleBindings(in).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: om("signingkey"),
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "sa", Namespace: in}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "signingkey"},
	}, metav1.CreateOptions{})
	allowed("bind its own Role", err)
	allowed("delete what it installed", helm.AppsV1().Deployments(in).Delete(ctx, "d", metav1.DeleteOptions{}))

	// Cluster scope.
	_, err = helm.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: om("chart-cr")}, metav1.CreateOptions{})
	refused("create a ClusterRole", err)
	_, err = helm.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: om("chart-crb"),
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"}}, metav1.CreateOptions{})
	refused("create a ClusterRoleBinding", err)
	_, err = helm.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: om("chart-ns")}, metav1.CreateOptions{})
	refused("create a Namespace", err)
	// Another namespace.
	for _, other := range []string{"tenant-other", layout.Kernel(layout.Secrets)} {
		_, err = helm.CoreV1().ConfigMaps(other).Create(ctx, &corev1.ConfigMap{ObjectMeta: om("c")}, metav1.CreateOptions{})
		refused("create a ConfigMap in "+other, err)
		_, err = helm.CoreV1().Secrets(other).List(ctx, metav1.ListOptions{})
		refused("read the Secrets of "+other, err)
	}
	// The platform's kinds in the tenant's own namespace.
	_, err = helm.NetworkingV1().NetworkPolicies(in).Create(ctx, &networkingv1.NetworkPolicy{ObjectMeta: om("allow-all")}, metav1.CreateOptions{})
	refused("create a NetworkPolicy", err)
	refused("delete the tenant's quota", helm.CoreV1().ResourceQuotas(in).Delete(ctx, "tenant-quota", metav1.DeleteOptions{}))
	refused("delete the tenant's limits", helm.CoreV1().LimitRanges(in).Delete(ctx, "tenant-limits", metav1.DeleteOptions{}))
	// Its own binding is a RoleBinding in its namespace, which RBAC cannot
	// set apart by name: a chart could remove it, which takes away its own
	// rights and no one else's (the operator puts it back), or add subjects to
	// it, which gives them what a Role of the chart's own could give them
	// anyway -- this role, in this namespace.
	// More than it holds, through a Role of its own.
	_, err = helm.RbacV1().Roles(in).Create(ctx, &rbacv1.Role{ObjectMeta: om("exec"), Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"},
	}}}, metav1.CreateOptions{})
	refused("create a Role granting more than it holds", err)
	_, err = helm.RbacV1().RoleBindings(in).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: om("admin"),
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "sa", Namespace: in}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
	}, metav1.CreateOptions{})
	refused("bind cluster-admin in its namespace", err)
}

func withRestart(p corev1.PodTemplateSpec, policy corev1.RestartPolicy) corev1.PodTemplateSpec {
	p.Spec.RestartPolicy = policy
	return p
}
