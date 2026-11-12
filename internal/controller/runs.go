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
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/dispatcher"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
	"github.com/gentian-org/gentian-os/internal/security"
)

// Runs (requires.services.runs). A component that declares the requirement
// is given a key for the dispatcher, written here once into the component's
// Secret; the dispatcher's table carries the key's hash and nothing else of
// it, as the rights check's (rights_check.go) and for the same reasons.
//
// What the operator prepares is state, and all of it: the key, the tenant's
// namespace for runs with its rules, and the table that says who may start
// what. Starting a run is the dispatcher's, and nothing here does it.
const (
	// runsCallerLabel marks the Secret a component's key for the dispatcher
	// is delivered in. The writer of the table lists by it.
	runsCallerLabel = "gentianos.io/runs-caller"
	// runsIngressLabel marks the rule, in a tenant's namespace, that lets a
	// caller's runs reach the caller.
	runsIngressLabel = "gentianos.io/runs-ingress"

	runsDefaultDeny    = "runs-default-deny"
	runsQuotaName      = "runs-quota"
	runsLimitsName     = "runs-limits"
	runsImagesPolicy   = "run-images"
	reasonRunsInvalid  = "RunsInvalid"
	runsNamespaceCheck = time.Minute
)

// What a tenant's runs are given when its plan says nothing: room for a few
// steps at once, and far from what its apps are given.
var (
	defaultRunsPods   = int64(10)
	defaultRunsCPU    = resource.MustParse("2")
	defaultRunsMemory = resource.MustParse("4Gi")
	// One attempt that states no resources of its own.
	defaultRunCPU    = resource.MustParse("250m")
	defaultRunMemory = resource.MustParse("256Mi")
)

func runsSecretName(component string) string { return "runs-" + component }

// runsKeys are the keys for the dispatcher.
var runsKeys = keySecret{label: runsCallerLabel, key: dispatcher.KeyKey, name: runsSecretName}

// wantsRuns reports whether the component starts runs.
func wantsRuns(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Runs != nil
}

// pinned is the build package.images pins for a repository, "" for none.
func pinned(profile *gentianov1alpha1.ComponentProfile, repository string) string {
	for _, image := range profile.Spec.Package.Images {
		if image.Repository == repository {
			return image.Digest
		}
	}
	return ""
}

// runsRefusal is why the profile's runs cannot be started, "" when they can
// or when it declares none. A run of the component's own names an image the
// profile pins: nothing runs at a build nobody stated.
func runsRefusal(profile *gentianov1alpha1.ComponentProfile) string {
	if !wantsRuns(profile) {
		return ""
	}
	for _, own := range profile.Services().Runs.Own {
		if pinned(profile, own.Image) == "" {
			return fmt.Sprintf("requires.services.runs.own[%s] names the image %s, which package.images does not pin", own.Name, own.Image)
		}
	}
	return ""
}

// ensureRuns keeps the Secret a component's key for the dispatcher is
// delivered in (ensureKeySecret), with the dispatcher's address.
func (r *ComponentReconciler) ensureRuns(ctx context.Context, comp *gentianov1alpha1.Component, tenantName string, profile *gentianov1alpha1.ComponentProfile) error {
	if !wantsRuns(profile) {
		return nil
	}
	return r.ensureKeySecret(ctx, comp, tenantName, runsKeys, map[string]string{dispatcher.URLKey: r.dispatcherURL()})
}

// DispatcherPodLabels are the dispatcher's pods' labels, as the chart writes
// them: what the way to it from a component selects.
func DispatcherPodLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": "gentian-os", "app.kubernetes.io/component": "dispatcher"}
}

// runsEgress is the way to the dispatcher: its pods, on its one port, and
// nothing else of the namespace the kernel's controllers run in.
func runsEgress() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(dispatcher.Port)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": layout.Kernel(layout.Control)},
			},
			PodSelector: &metav1.LabelSelector{MatchLabels: DispatcherPodLabels()},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}

// runnableOf is what one attempt of a declared run is given.
func runnableOf(name, version, image, digest string, command []string, deadline int32, resources *gentianov1alpha1.ActionResources) dispatcher.Runnable {
	out := dispatcher.Runnable{
		Name: name, Version: version, Image: image, Digest: digest, Command: command,
		Runtime: dispatcher.RuntimeContainer, DeadlineSeconds: deadline,
	}
	if out.DeadlineSeconds == 0 {
		out.DeadlineSeconds = 600
	}
	if resources != nil {
		if resources.CPU != nil {
			out.CPU = resources.CPU.String()
		}
		if resources.Memory != nil {
			out.Memory = resources.Memory.String()
		}
	}
	return out
}

// runCaller is one component that may start runs, with what the rules of
// its tenant's two namespaces need to know of it.
type runCaller struct {
	entry dispatcher.Caller
	// port is the port of the component's pods the callback Service sends
	// to, nil while the Service is not there or has no such port.
	port *intstr.IntOrString
}

// installedAction is one action a tenant has installed and that is Ready.
type installedAction struct {
	comp     *gentianov1alpha1.Component
	profile  *gentianov1alpha1.ComponentProfile
	runnable dispatcher.Runnable
}

// tenantRuns is everything about runs in one tenant.
type tenantRuns struct {
	tenant  *gentianov1alpha1.Tenant
	callers []runCaller
	actions []installedAction
}

// RunsReconciler keeps what a run needs before it starts: each tenant's
// namespace for runs, and the dispatcher's table.
type RunsReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it reads. Nil holds nothing.
	Definitions *crdcheck.Holder
	client.Client
	// ProgramImage is the image the run program is taken from, which every
	// run's pod names beside its own.
	ProgramImage string
	// Now is the time grants are read at; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps;services;secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kyverno.io,resources=policies,verbs=get;list;watch;create;update;patch;delete

var runsRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: dispatcher.TableConfigMap}}

func (r *RunsReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// gather reads who may start runs and what may run, by tenant.
func (r *RunsReconciler) gather(ctx context.Context) (map[string]*tenantRuns, error) {
	out := map[string]*tenantRuns{}
	of := func(tenant *gentianov1alpha1.Tenant) *tenantRuns {
		if out[tenant.Name] == nil {
			out[tenant.Name] = &tenantRuns{tenant: tenant}
		}
		return out[tenant.Name]
	}

	keys, err := componentKeys(ctx, r.Client, runsKeys, wantsRuns)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if refusal := runsRefusal(k.profile); refusal != "" {
			// The Component is held for it (runsRefusal in its reconcile).
			continue
		}
		requirement := k.profile.Services().Runs
		caller := runCaller{entry: dispatcher.Caller{
			Callback: dispatcher.Callback{
				Service: requirement.Callback.Service, Namespace: k.namespace,
				Port: requirement.Callback.Port, Path: requirement.Callback.Path,
			},
			Concurrency: requirement.Concurrency,
		}}
		caller.entry.Tenant, caller.entry.Component, caller.entry.KeyHash = k.Tenant, k.Component, k.KeyHash
		if caller.entry.Concurrency == 0 {
			caller.entry.Concurrency = 4
		}
		for _, own := range requirement.Own {
			caller.entry.Own = append(caller.entry.Own, runnableOf(
				own.Name, k.profile.Spec.Version, own.Image, pinned(k.profile, own.Image),
				own.Command, own.DeadlineSeconds, own.Resources))
		}
		if caller.port, err = r.callbackPort(ctx, caller.entry.Callback); err != nil {
			return nil, err
		}
		of(k.holder).callers = append(of(k.holder).callers, caller)
	}

	components := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, components); err != nil {
		return nil, err
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return nil, err
	}
	for i := range components.Items {
		comp := &components.Items[i]
		if comp.Spec.Class != gentianov1alpha1.ComponentClassAction || comp.DeletionTimestamp != nil ||
			!meta.IsStatusConditionTrue(comp.Status.Conditions, "Ready") {
			continue
		}
		holder := gentianov1alpha1.TenantInNamespace(tenants.Items, comp.Namespace)
		if holder == nil {
			continue
		}
		profile, err := getProfileFor(ctx, r.Client, comp.Spec.ProfileRef.Name, comp.Spec.ProfileRef.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if !classIncludes(profile, gentianov1alpha1.ComponentClassAction) || profile.Spec.Action == nil || len(profile.Spec.Package.Images) != 1 {
			continue
		}
		image := profile.Spec.Package.Images[0]
		of(holder).actions = append(of(holder).actions, installedAction{
			comp: comp, profile: profile,
			runnable: runnableOf(comp.Name, profile.Spec.Version, image.Repository, image.Digest,
				profile.Spec.Action.Command, profile.Spec.Action.DeadlineSeconds, profile.Spec.Action.Resources),
		})
	}
	for _, runs := range out {
		sort.Slice(runs.actions, func(i, j int) bool { return runs.actions[i].runnable.Name < runs.actions[j].runnable.Name })
	}
	return out, nil
}

// callbackPort is the port of the caller's pods its callback Service sends
// to. A rule between pods names the pods' port, and the Service's own port
// is only what a client dials.
func (r *RunsReconciler) callbackPort(ctx context.Context, callback dispatcher.Callback) (*intstr.IntOrString, error) {
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: callback.Service, Namespace: callback.Namespace}, service); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	for _, port := range service.Spec.Ports {
		if port.Port != callback.Port {
			continue
		}
		target := port.TargetPort
		if target.Type == intstr.Int && target.IntVal == 0 {
			target = intstr.FromInt32(port.Port)
		}
		return &target, nil
	}
	return nil, nil
}

func (r *RunsReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	gathered, err := r.gather(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// A namespace for runs is made for a tenant that has a caller, and kept
	// in order for as long as it exists: a tenant whose last caller went has
	// one in which nothing is let through to anything.
	existing := &corev1.NamespaceList{}
	if err := r.List(ctx, existing, client.MatchingLabels{layout.LabelTier: string(layout.TierTenantRuns)}); err != nil {
		return ctrl.Result{}, err
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return ctrl.Result{}, err
	}
	living := map[string]bool{}
	for i := range tenants.Items {
		living[tenants.Items[i].Name] = tenants.Items[i].DeletionTimestamp == nil
	}
	have := map[string]bool{}
	for i := range existing.Items {
		ns := &existing.Items[i]
		// The namespace of a tenant that is gone, or going, goes with it:
		// the operator made it, and nothing else would take it away. Only
		// one that is named and labeled as the operator names and labels
		// them is the operator's to delete.
		tenant := ns.Labels[layout.LabelTenant]
		if !living[tenant] {
			if ns.DeletionTimestamp == nil && ns.Name == layout.TenantRuns(tenant) &&
				ns.Labels["app.kubernetes.io/managed-by"] == managedByValue {
				if err := r.Delete(ctx, ns); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
			}
			continue
		}
		have[ns.Name] = true
	}

	table := dispatcher.Table{}
	for i := range tenants.Items {
		tenant := &tenants.Items[i]
		runs := gathered[tenant.Name]
		if runs == nil {
			runs = &tenantRuns{tenant: tenant}
		}
		namespace := layout.TenantRuns(tenant.Name)
		if len(runs.callers) == 0 && !have[namespace] {
			continue
		}
		if tenant.DeletionTimestamp != nil {
			continue
		}
		if err := r.ensureRunNamespace(ctx, runs, namespace); err != nil {
			return ctrl.Result{}, fmt.Errorf("the namespace for runs of %s: %w", tenant.Name, err)
		}
		if len(runs.callers) == 0 {
			continue
		}
		entry := dispatcher.Tenant{Tenant: tenant.Name, Namespace: namespace}
		for _, action := range runs.actions {
			entry.Actions = append(entry.Actions, action.runnable)
		}
		table.Tenants = append(table.Tenants, entry)
		for _, caller := range runs.callers {
			table.Callers = append(table.Callers, caller.entry)
		}
	}
	if err := r.writeTable(ctx, table); err != nil {
		return ctrl.Result{}, err
	}
	// A grant runs out and a callback Service arrives without an event this
	// reconciler is told of.
	return ctrl.Result{RequeueAfter: runsNamespaceCheck}, nil
}

func (r *RunsReconciler) writeTable(ctx context.Context, table dispatcher.Table) error {
	body, err := table.Document()
	if err != nil {
		return err
	}
	if _, err := dispatcher.ParseTable([]byte(body)); err != nil {
		// What the dispatcher would refuse is not written over what it has.
		logf.FromContext(ctx).Error(err, "the dispatcher's table as gathered is not one; the last one stays")
		return nil
	}
	cm := &corev1.ConfigMap{Data: map[string]string{dispatcher.TableKey: body}}
	cm.Name, cm.Namespace = dispatcher.TableConfigMap, layout.Kernel(layout.Control)
	cm.Labels = map[string]string{managedByLabel: managedByValue}
	return ensureObject(ctx, r.Client, cm, func(existing *corev1.ConfigMap) bool {
		if equality.Semantic.DeepEqual(existing.Data, cm.Data) {
			return false
		}
		existing.Data = cm.Data
		return true
	})
}

func (r *RunsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toTable := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{runsRequest}
	})
	callers := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetLabels()[runsCallerLabel] == "true" })
	runNamespaces := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[layout.LabelTier] == string(layout.TierTenantRuns)
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("runs").
		Watches(&gentianov1alpha1.Component{}, toTable).
		Watches(&gentianov1alpha1.ComponentProfile{}, toTable).
		Watches(&gentianov1alpha1.Tenant{}, toTable).
		Watches(&corev1.Secret{}, toTable, builder.WithPredicates(callers)).
		Watches(&corev1.Namespace{}, toTable, builder.WithPredicates(runNamespaces)).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.Component{}, r))
}

// -- the namespace for runs ---------------------------------------------------

// ensureRunNamespace keeps a tenant's namespace for runs and everything in
// it that is state: who a pod there may reach, how much may run, which
// images, and under which account. The pods are the dispatcher's.
func (r *RunsReconciler) ensureRunNamespace(ctx context.Context, runs *tenantRuns, namespace string) error {
	tenantNamespace := tenantNamespaceName(runs.tenant)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{
		layout.LabelTier:               string(layout.TierTenantRuns),
		layout.LabelTenant:             runs.tenant.Name,
		"app.kubernetes.io/managed-by": managedByValue,
		// A run is somebody's code started on somebody's word: it runs
		// under the strictest standard, and no profile can ask for less.
		"pod-security.kubernetes.io/enforce": "restricted",
	}}}
	if err := r.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	labels := map[string]string{managedByLabel: managedByValue, layout.LabelTenant: runs.tenant.Name}
	no := false
	account := &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: dispatcher.ServiceAccount, Namespace: namespace, Labels: labels},
		AutomountServiceAccountToken: &no,
	}
	if err := ensureObject(ctx, r.Client, account, func(existing *corev1.ServiceAccount) bool {
		if existing.AutomountServiceAccountToken != nil && !*existing.AutomountServiceAccountToken {
			return false
		}
		existing.AutomountServiceAccountToken = &no
		return true
	}); err != nil {
		return err
	}

	quota, limits := runQuota(runs.tenant, namespace, labels)
	if err := ensureObject(ctx, r.Client, quota, func(existing *corev1.ResourceQuota) bool {
		if equality.Semantic.DeepEqual(existing.Spec.Hard, quota.Spec.Hard) {
			return false
		}
		existing.Spec.Hard = quota.Spec.Hard
		return true
	}); err != nil {
		return err
	}
	if err := ensureObject(ctx, r.Client, limits, func(existing *corev1.LimitRange) bool {
		if equality.Semantic.DeepEqual(existing.Spec, limits.Spec) {
			return false
		}
		existing.Spec = limits.Spec
		return true
	}); err != nil {
		return err
	}

	if err := ensureSpec(ctx, r.Client, runImagesPolicy(namespace, r.allowedImages(runs))); err != nil {
		return fmt.Errorf("ensure Policy %s: %w", runsImagesPolicy, err)
	}

	// The rules of the namespace itself, and what is no longer one of them.
	desired := runNamespacePolicies(runs, namespace, tenantNamespace, labels, r.now())
	if err := r.applyPolicies(ctx, namespace, desired, client.MatchingLabels{managedByLabel: managedByValue}); err != nil {
		return err
	}
	// The other end of a caller's rule: its pods admit its runs.
	return r.applyPolicies(ctx, tenantNamespace, callerIngressPolicies(runs, namespace, tenantNamespace),
		client.MatchingLabels{runsIngressLabel: "true"})
}

// applyPolicies writes the policies and deletes every other one the
// selector finds in the namespace.
func (r *RunsReconciler) applyPolicies(ctx context.Context, namespace string, desired []*networkingv1.NetworkPolicy, mine client.MatchingLabels) error {
	keep := map[string]bool{}
	for _, policy := range desired {
		keep[policy.Name] = true
		if err := ensureObject(ctx, r.Client, policy, func(existing *networkingv1.NetworkPolicy) bool {
			if equality.Semantic.DeepEqual(existing.Spec, policy.Spec) && equality.Semantic.DeepEqual(existing.Labels, policy.Labels) {
				return false
			}
			existing.Spec, existing.Labels = policy.Spec, policy.Labels
			return true
		}); err != nil {
			return err
		}
	}
	found := &networkingv1.NetworkPolicyList{}
	if err := r.List(ctx, found, client.InNamespace(namespace), mine); err != nil {
		return err
	}
	for i := range found.Items {
		if keep[found.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &found.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// allowedImages are the builds that may run in the tenant's namespace for
// runs: its installed actions, its callers' own runs, and the run program.
func (r *RunsReconciler) allowedImages(runs *tenantRuns) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	add(r.ProgramImage)
	for _, action := range runs.actions {
		add(action.runnable.Ref())
	}
	for _, caller := range runs.callers {
		for _, own := range caller.entry.Own {
			add(own.Ref())
		}
	}
	sort.Strings(out)
	return out
}

// runImagesPolicy admits, in a namespace for runs, a pod whose every image
// is one of the allowed builds and refuses any other, whoever asks. The
// dispatcher starts nothing else; this is what holds if it were made to.
func runImagesPolicy(namespace string, allowed []string) *unstructured.Unstructured {
	values := []interface{}{}
	for _, ref := range allowed {
		values = append(values, ref)
	}
	if len(values) == 0 {
		// No image is the empty name, so nothing is admitted.
		values = append(values, "")
	}
	var each []interface{}
	for _, list := range []string{
		"request.object.spec.containers",
		"request.object.spec.initContainers || `[]`",
		"request.object.spec.ephemeralContainers || `[]`",
	} {
		each = append(each, map[string]interface{}{
			"list": list,
			"deny": map[string]interface{}{"conditions": map[string]interface{}{"all": []interface{}{
				map[string]interface{}{"key": "{{ element.image }}", "operator": "AnyNotIn", "value": values},
			}}},
		})
	}
	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      runsImagesPolicy,
			"namespace": namespace,
			"labels":    map[string]interface{}{managedByLabel: managedByValue},
			"annotations": map[string]interface{}{
				"pod-policies.kyverno.io/autogen-controllers": "none",
			},
		},
		"spec": map[string]interface{}{
			"validationFailureAction": "Enforce",
			"failurePolicy":           "Fail",
			"background":              false,
			"rules": []interface{}{map[string]interface{}{
				"name": "run-images",
				"match": map[string]interface{}{"any": []interface{}{map[string]interface{}{
					"resources": map[string]interface{}{"kinds": []interface{}{"Pod", "Pod/ephemeralcontainers"}},
				}}},
				"validate": map[string]interface{}{
					"message": "only an installed action, a caller's own run and the run program run here, each at its pinned build",
					"foreach": each,
				},
			}},
		},
	}}
	policy.SetGroupVersionKind(kyvernoPolicyGVK)
	return policy
}

// runQuota is how much may run in a tenant's namespace for runs, and what
// one attempt is given when it states nothing. A run reserves what it may
// use, so requests and limits are held to the same.
func runQuota(tenant *gentianov1alpha1.Tenant, namespace string, labels map[string]string) (*corev1.ResourceQuota, *corev1.LimitRange) {
	pods, cpu, memory := defaultRunsPods, defaultRunsCPU, defaultRunsMemory
	if q := tenant.Spec.Quotas; q != nil {
		if q.RunsMaxPods > 0 {
			pods = int64(q.RunsMaxPods)
		}
		if q.RunsCPU != nil {
			cpu = *q.RunsCPU
		}
		if q.RunsMemory != nil {
			memory = *q.RunsMemory
		}
	}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: runsQuotaName, Namespace: namespace, Labels: labels},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePods:           *resource.NewQuantity(pods, resource.DecimalSI),
			corev1.ResourceRequestsCPU:    cpu,
			corev1.ResourceLimitsCPU:      cpu,
			corev1.ResourceRequestsMemory: memory,
			corev1.ResourceLimitsMemory:   memory,
		}},
	}
	one := corev1.ResourceList{corev1.ResourceCPU: defaultRunCPU, corev1.ResourceMemory: defaultRunMemory}
	limits := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: runsLimitsName, Namespace: namespace, Labels: labels},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: corev1.LimitTypeContainer, Default: one, DefaultRequest: one,
		}}},
	}
	return quota, limits
}

func runPolicy(name, namespace string, labels map[string]string, pods map[string]string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: pods},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
}

// runNamespacePolicies are the rules of a namespace for runs. Nothing is
// let in. Every pod may ask the cluster's DNS and nothing else; a run of an
// installed action is given the egress granted to that action; and a run is
// let through to the component that started it, on the port it calls back
// to. No rule reaches an adapter, an engine or the kernel.
func runNamespacePolicies(runs *tenantRuns, namespace, tenantNamespace string, labels map[string]string, now time.Time) []*networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(53)
	deny := runPolicy(runsDefaultDeny, namespace, labels, nil)
	deny.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	deny.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
	}}
	out := []*networkingv1.NetworkPolicy{deny}

	for _, action := range runs.actions {
		granted := security.GrantedEgressRules(action.profile, action.comp, now)
		if len(granted) == 0 {
			continue
		}
		policy := runPolicy("action-"+action.runnable.Name, namespace, labels, map[string]string{dispatcher.LabelAction: action.runnable.Name})
		policy.Spec.Egress = granted
		out = append(out, policy)
	}
	for _, caller := range runs.callers {
		if caller.port == nil {
			continue
		}
		policy := runPolicy("caller-"+caller.entry.Component, namespace, labels, map[string]string{dispatcher.LabelCaller: caller.entry.Component})
		policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": tenantNamespace}},
				// The component's pods by the label the platform writes on
				// them, never by what its Service selects: a Service is the
				// chart's to write, and could select another app's pods.
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabel: caller.entry.Component}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: caller.port}},
		}}
		out = append(out, policy)
	}
	return out
}

// callerIngressPolicies are, in the tenant's own namespace, the rules by
// which each caller's pods admit that caller's runs and nobody else's.
func callerIngressPolicies(runs *tenantRuns, namespace, tenantNamespace string) []*networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	var out []*networkingv1.NetworkPolicy
	for _, caller := range runs.callers {
		if caller.port == nil {
			continue
		}
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "runs-" + caller.entry.Component, Namespace: tenantNamespace,
				Labels: map[string]string{
					managedByLabel: managedByValue, runsIngressLabel: "true",
					layout.LabelTenant: runs.tenant.Name, appLabel: caller.entry.Component,
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{appLabel: caller.entry.Component}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{dispatcher.LabelCaller: caller.entry.Component}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: caller.port}},
				}},
			},
		})
	}
	return out
}
