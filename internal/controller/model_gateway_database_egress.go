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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// The egress policy of the model gateway's PostgreSQL, the CloudNativePG
// Cluster litellm-db in the gateway's namespace
// (kernel/services/llm/manifests/templates/llm-services.yaml).
//
// The gateway's chart writes every other policy of that namespace. This one is
// written here because its instances reach the Kubernetes API, and the
// addresses the API answers on are the cluster's: the operator reads them from
// the kubernetes Service's EndpointSlice, as it does for a tenant's namespace
// (netpolicy.BaselineEgress), and rewrites the policy when they change.
//
// An instance may reach:
//
//   - DNS, on 53;
//   - the API server: KUBE_APISERVER_CIDR on 443, and each address and port of
//     the kubernetes EndpointSlice. The instance manager watches its Cluster
//     and writes its status there;
//   - the cluster's own pods on the data port: a replica streams from the
//     primary, and a former primary is rewound from the new one (pg_rewind),
//     both through the litellm-db-rw Service.
//
// Nothing else. Its status port is asked by CloudNativePG's operator, never by
// another instance. Its initdb and join Jobs carry no podRole and are not
// selected.
//
// It exists while the cluster serves models (llm.enabled of the claim) and the
// switch for the policies on the system tier's servers is on
// (STORE_NETWORK_POLICIES, the value the gateway's chart is handed as
// networkPolicy.enabled). Otherwise it is removed.
//
// The namespace is synced by an Argo CD Application with pruning on. Argo CD
// tracks what it applied by its tracking annotation, which this policy never
// carries, and the chart renders no object of this name: Argo CD neither
// prunes it nor reverts it.
const (
	modelGatewayDatabaseEgressName = "llm-database-egress"
	// The CloudNativePG Cluster of the gateway, and the port its instances
	// serve PostgreSQL on.
	modelGatewayDatabaseCluster       = "litellm-db"
	modelGatewayDatabasePort    int32 = 5432
)

// StoreNetworkPoliciesEnabled reports the cluster's switch for the policies
// on the system tier's servers, as the operator is told it
// (STORE_NETWORK_POLICIES, from the installer through the chart). The model
// gateway's chart reads the same value, handed by its ApplicationSet. Anything
// but "false" is on, as the charts read it.
func StoreNetworkPoliciesEnabled() bool {
	return os.Getenv("STORE_NETWORK_POLICIES") != "false"
}

// ModelGatewayDatabaseEgressReconciler keeps llm-database-egress.
type ModelGatewayDatabaseEgressReconciler struct {
	client.Client
	// NetworkPolicies is the switch (StoreNetworkPoliciesEnabled).
	NetworkPolicies bool
	// KubeAPIServerCIDR is the range the API server is admitted in on 443
	// (KubeAPIServerCIDR).
	KubeAPIServerCIDR string
}

var modelGatewayDatabaseEgressRequest = reconcile.Request{NamespacedName: types.NamespacedName{
	Name: modelGatewayDatabaseEgressName, Namespace: llmNamespace,
}}

func (r *ModelGatewayDatabaseEgressReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: llmNamespace}, ns); err != nil {
		// No namespace, no policy: its arrival is watched.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.NetworkPolicies || !clusterLLMEnabled(ctx, r.Client) || ns.DeletionTimestamp != nil {
		policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Name: modelGatewayDatabaseEgressName, Namespace: llmNamespace,
		}}
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, policy))
	}
	desired := modelGatewayDatabaseEgressPolicy(r.KubeAPIServerCIDR, loadKubeAPIEndpointSlice(ctx, r.Client))
	return ctrl.Result{}, ensureObject(ctx, r.Client, desired, func(existing *networkingv1.NetworkPolicy) bool {
		if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
			equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
			return false
		}
		existing.Labels, existing.Spec = desired.Labels, desired.Spec
		return true
	})
}

// modelGatewayDatabaseEgressPolicy is the policy, for the API server's range
// and the kubernetes Service's EndpointSlice (nil when there is none).
func modelGatewayDatabaseEgressPolicy(apiServerCIDR string, kubeAPIEndpts *discoveryv1.EndpointSlice) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(modelGatewayDatabasePort)
	egress := netpolicy.BaselineEgress(netpolicy.Config{KubeAPIServerCIDR: apiServerCIDR}, kubeAPIEndpts)
	egress = append(egress, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{cnpgClusterLabel: modelGatewayDatabaseCluster},
		}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	})
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelGatewayDatabaseEgressName,
			Namespace: llmNamespace,
			Labels: map[string]string{
				managedByLabel:          managedByValue,
				"gentianos.io/tier":     "system",
				"gentianos.io/function": "llm",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				cnpgClusterLabel: modelGatewayDatabaseCluster,
				cnpgPodRoleLabel: cnpgPodRoleInstance,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// CloudNativePG's labels on an instance pod.
const (
	cnpgClusterLabel    = "cnpg.io/cluster"
	cnpgPodRoleLabel    = "cnpg.io/podRole"
	cnpgPodRoleInstance = "instance"
)

func (r *ModelGatewayDatabaseEgressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toPolicy := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{modelGatewayDatabaseEgressRequest}
	})
	named := func(namespace, name string) predicate.Predicate {
		return predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetNamespace() == namespace && o.GetName() == name
		})
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("model-gateway-database-egress").
		// The API server's addresses.
		Watches(&discoveryv1.EndpointSlice{}, toPolicy, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetLabels()[kubeAPIServiceNameLabel] == kubeAPIServiceName && o.GetNamespace() == metav1.NamespaceDefault
		}))).
		// Whether the cluster serves models.
		Watches(&corev1.ConfigMap{}, toPolicy, builder.WithPredicates(named(clusterConfigNamespace, clusterConfigName))).
		Watches(&corev1.Namespace{}, toPolicy, builder.WithPredicates(named("", llmNamespace))).
		// The policy itself, changed or removed by someone else.
		Watches(&networkingv1.NetworkPolicy{}, toPolicy, builder.WithPredicates(named(llmNamespace, modelGatewayDatabaseEgressName))).
		Complete(r)
}
