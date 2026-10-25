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
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The app authorization engine (app-authorization.md §1, §2.3): the app
// instance of OpenFGA and the arbiter in system-authorization, for a cluster
// on which a component declares an authorization store.
//
// The Cluster composition composes the namespace and the instance's Release
// once the engine is in the operator's set (system_engines.go). What the
// instance needs that is a credential or a workload of the platform's own is
// made here, as the operator makes every component's stores:
//
//   - its database and role on the tenants' PostgreSQL, by the role Job every
//     database is made with (buildRoleScript), with a password made once and
//     kept in the Secret the instance reads its datastore URI from;
//   - the arbiter, from the operator's own image with its own command and
//     ServiceAccount, which may read and write the record of its stores and
//     the instance's key, and nothing else;
//   - the two NetworkPolicies of the namespace: the instance admits the
//     arbiter and nothing else, and the arbiter admits the tenants' namespaces
//     on its one port, which their own policies narrow to the components that
//     declared a store.
//
// The instance's key is not made here. The arbiter makes it on its first
// start (arbiter.EnsureInstanceKey), so that nothing else is ever handed it.
const (
	// arbiterRoleJob is the Job that makes the instance's role and database.
	arbiterRoleJob = "openfga-apps-role"
	// openfgaAppsDatabase is the instance's database, and its role's name.
	openfgaAppsDatabase = "openfga_apps"
	// roleProvisionedAnnotation on the datastore Secret records that the
	// role Job succeeded with the password in it; the Job is not run again
	// for as long as the Secret is that one.
	roleProvisionedAnnotation = "gentianos.io/role-provisioned"
)

// AuthorizationEngineReconciler keeps what the app instance and the arbiter
// need, while the engine is wanted.
type AuthorizationEngineReconciler struct {
	client.Client
	// Namespace is the control namespace, where the engine set is.
	Namespace string
	// Image is the operator's own image, which the arbiter runs.
	Image string
}

var authorizationEngineRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: arbiter.ServiceName}}

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

func (r *AuthorizationEngineReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	set := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: systemEnginesConfigMap, Namespace: r.Namespace}, set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if set.Data[EngineAuthorization] != engineWanted {
		// Never wanted, or purged: what a purge leaves goes with the
		// namespace the composition stops composing.
		return ctrl.Result{}, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: arbiter.Namespace}, ns); err != nil {
		if errors.IsNotFound(err) {
			// Composed next; until then there is nowhere to write.
			return ctrl.Result{RequeueAfter: componentRequeue}, nil
		}
		return ctrl.Result{}, err
	}
	if r.Image == "" {
		return ctrl.Result{}, fmt.Errorf("no image for the arbiter: GENTIAN_IMAGE is not set")
	}
	password, err := r.ensureDatastore(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("the app instance's datastore: %w", err)
	}
	if done, err := r.ensureRole(ctx, password); err != nil {
		return ctrl.Result{}, fmt.Errorf("the app instance's role: %w", err)
	} else if !done {
		return ctrl.Result{RequeueAfter: componentRequeue}, nil
	}
	for _, obj := range arbiterObjects(r.Image) {
		if err := r.apply(ctx, obj); err != nil {
			return ctrl.Result{}, fmt.Errorf("%T %s: %w", obj, obj.GetName(), err)
		}
	}
	return ctrl.Result{}, nil
}

// ensureDatastore is the role's password, from the Secret the instance reads
// its datastore URI from, made the first time and kept.
func (r *AuthorizationEngineReconciler) ensureDatastore(ctx context.Context) (string, error) {
	key := types.NamespacedName{Name: arbiter.DatastoreSecret, Namespace: arbiter.Namespace}
	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if err == nil {
		if pw := string(existing.Data["password"]); pw != "" {
			return pw, nil
		}
	} else if !errors.IsNotFound(err) {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	// URL-safe without padding, so the password needs no escaping in the URI.
	pw := base64.RawURLEncoding.EncodeToString(raw)
	uri := fmt.Sprintf("postgres://%s:%s@%s-rw.%s.svc.cluster.local:5432/%s?sslmode=disable",
		openfgaAppsDatabase, pw, cnpgClusterName, postgresNamespace, openfgaAppsDatabase)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{managedByLabel: managedByValue}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"uri": []byte(uri), "password": []byte(pw)},
	}
	if err == nil {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Data, existing.Annotations = secret.Data, nil
		return pw, r.Patch(ctx, existing, patch)
	}
	return pw, r.Create(ctx, secret)
}

// ensureRole runs the role Job until it has succeeded once with the
// password the Secret holds, and reports whether it has.
func (r *AuthorizationEngineReconciler) ensureRole(ctx context.Context, password string) (bool, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: arbiter.DatastoreSecret, Namespace: arbiter.Namespace}, secret); err != nil {
		return false, err
	}
	if secret.Annotations[roleProvisionedAnnotation] == "true" {
		return true, nil
	}
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: arbiterRoleJob, Namespace: postgresNamespace}, job)
	if errors.IsNotFound(err) {
		job = makeAppInstanceRoleJob()
		if err := r.Create(ctx, job); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	if err := ensureJobSecret(ctx, r.Client, job, jobSecret(job, passwordSecretData(password))); err != nil {
		return false, err
	}
	switch {
	case job.Status.Succeeded > 0:
		patch := client.MergeFrom(secret.DeepCopy())
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[roleProvisionedAnnotation] = "true"
		return true, r.Patch(ctx, secret, patch)
	case jobFailed(job):
		// Run again, from a new Job: the script is idempotent.
		prop := metav1.DeletePropagationBackground
		return false, client.IgnoreNotFound(r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}))
	}
	return false, nil
}

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// makeAppInstanceRoleJob is the role Job every database on the tenants'
// PostgreSQL is made with, for the instance's role and database.
func makeAppInstanceRoleJob() *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	container := psqlContainer("provision-role",
		buildRoleScript(openfgaAppsDatabase, openfgaAppsDatabase, gentianov1alpha1.SchemaPreferencePublic, false), "")
	container.Env = append(container.Env, meta.SecretEnv("ROLE_PW", arbiterRoleJob, jobSecretPasswordKey))
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: arbiterRoleJob, Namespace: postgresNamespace,
			Labels: map[string]string{managedByLabel: managedByValue, "app.kubernetes.io/part-of": arbiter.ServiceName},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyOnFailure, Containers: []corev1.Container{container}},
			},
		},
	}
}

// arbiterObjects are the arbiter's workload and the namespace's policies.
func arbiterObjects(image string) []client.Object {
	ns := arbiter.Namespace
	labels := arbiterPodLabels()
	objLabels := map[string]string{managedByLabel: managedByValue, "app.kubernetes.io/name": arbiter.ServiceName}
	om := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: objLabels}
	}
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(arbiter.Port)
	replicas := int32(1)
	no, yes := false, true
	uid := int64(65532)

	sa := &corev1.ServiceAccount{ObjectMeta: om(arbiter.ServiceName)}
	// What the arbiter may do with the API server: keep the record of its
	// stores and make and read the instance's key. A ConfigMap or Secret
	// cannot be created by name, so create is the namespace's; read and
	// write are those two objects'.
	role := &rbacv1.Role{
		ObjectMeta: om(arbiter.ServiceName),
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{arbiter.StoresConfigMap}, Verbs: []string{"get", "update"}},
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{arbiter.KeySecret}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: om(arbiter.ServiceName),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: arbiter.ServiceName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: arbiter.ServiceName, Namespace: ns}},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: om(arbiter.ServiceName),
		Spec: appsv1.DeploymentSpec{
			// One: the arbiter creates a store on first sight, and two
			// replicas would agree on one only through the record.
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: arbiter.ServiceName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &yes, RunAsUser: &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    arbiter.ServiceName,
						Image:   image,
						Command: []string{"/arbiter"},
						Env: []corev1.EnvVar{
							{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
							{Name: "ARBITER_TABLE", Value: "/etc/arbiter/" + arbiter.TableKey},
						},
						Ports: []corev1.ContainerPort{
							{Name: "api", ContainerPort: arbiter.Port, Protocol: tcp},
							{Name: "health", ContainerPort: arbiter.HealthPort, Protocol: tcp},
						},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz", Port: intstr.FromString("health")}}, PeriodSeconds: 5},
						LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz", Port: intstr.FromString("health")}}, PeriodSeconds: 20},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "table", MountPath: "/etc/arbiter", ReadOnly: true}},
					}},
					// The operator's table. Optional: before the first
					// component's key is written the arbiter refuses every key.
					Volumes: []corev1.Volume{{Name: "table", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: arbiter.TableConfigMap}, Optional: &yes,
					}}}},
				},
			},
		},
	}
	service := &corev1.Service{
		ObjectMeta: om(arbiter.ServiceName),
		Spec: corev1.ServiceSpec{
			Selector: labels,
			// The same number as the pod's: a NetworkPolicy is matched
			// after the Service's translation and names the pod's port.
			Ports: []corev1.ServicePort{{Name: "api", Port: arbiter.Port, TargetPort: port, Protocol: tcp}},
		},
	}
	arbiterIngress := &networkingv1.NetworkPolicy{
		ObjectMeta: om("arbiter-ingress"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"gentianos.io/tier": "tenant"},
				}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
	instancePort := intstr.FromInt32(8080)
	instanceIngress := &networkingv1.NetworkPolicy{
		ObjectMeta: om("openfga-apps-ingress"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name": "openfga", "app.kubernetes.io/instance": arbiter.InstanceRelease,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			// The arbiter, on the HTTP API, and nothing else: not a
			// component, whose key the instance would not know but whose
			// connection is one more way to it than there needs to be.
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: labels}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &instancePort}},
			}},
		},
	}
	return []client.Object{sa, role, binding, deployment, service, arbiterIngress, instanceIngress}
}

// apply writes obj as desired, over what an earlier reconcile left.
func (r *AuthorizationEngineReconciler) apply(ctx context.Context, desired client.Object) error {
	existing, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("not an object")
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.SetLabels(desired.GetLabels())
		switch e := existing.(type) {
		case *corev1.ServiceAccount:
		case *rbacv1.Role:
			e.Rules = desired.(*rbacv1.Role).Rules
		case *rbacv1.RoleBinding:
			e.RoleRef, e.Subjects = desired.(*rbacv1.RoleBinding).RoleRef, desired.(*rbacv1.RoleBinding).Subjects
		case *appsv1.Deployment:
			e.Spec = desired.(*appsv1.Deployment).Spec
		case *corev1.Service:
			d := desired.(*corev1.Service)
			e.Spec.Selector, e.Spec.Ports = d.Spec.Selector, d.Spec.Ports
		case *networkingv1.NetworkPolicy:
			e.Spec = desired.(*networkingv1.NetworkPolicy).Spec
		}
		return nil
	})
	return err
}

// arbiterReady reports whether the arbiter's Deployment is ready.
func arbiterReady(ctx context.Context, c client.Reader) (bool, error) {
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: arbiter.ServiceName, Namespace: arbiter.Namespace}, dep); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return deploymentIsReady(dep), nil
}

func (r *AuthorizationEngineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toEngine := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{authorizationEngineRequest}
	})
	inNamespace := func(ns string) predicate.Predicate {
		return predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == ns })
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("authorization-engine").
		Watches(&corev1.ConfigMap{}, toEngine, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetName() == systemEnginesConfigMap && o.GetNamespace() == r.Namespace
		}))).
		Watches(&corev1.Namespace{}, toEngine, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetName() == arbiter.Namespace
		}))).
		Watches(&appsv1.Deployment{}, toEngine, builder.WithPredicates(inNamespace(arbiter.Namespace))).
		Watches(&batchv1.Job{}, toEngine, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetName() == arbiterRoleJob
		}))).
		Complete(r)
}
