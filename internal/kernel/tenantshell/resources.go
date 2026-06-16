/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the permissions and limitations under the License.
*/

// Package tenantshell builds Kubernetes namespace scaffolding for a tenant.
// Crossplane tenant-default Composition is the production owner; this package
// is shared by envtest simulation and documents the canonical resource shape.
package tenantshell

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	TenantLabel     = "gentianos.io/tenant"
	ManagedByLabel  = "app.kubernetes.io/managed-by"
	ManagedByValue  = "gentian-os"
	KernelNamespace = "platform-kernel"
	IngressNamespace = "ingress"
	EnvoyGatewayInstallNamespace = "envoy-gateway-system"
	RoutingModeGateway = "gateway"
)

// Config carries cluster-specific namespace names and routing options used when
// building tenant isolation NetworkPolicies.
type Config struct {
	InfraNamespace    string
	ServicesNamespace string
	OpenbaoNamespace  string
	RoutingMode       string
	KubeAPIServerCIDR string
}

// DefaultConfig returns dev-cluster defaults matching the operator env vars.
func DefaultConfig() Config {
	return Config{
		InfraNamespace:    "gentian-infra-dev",
		ServicesNamespace: "gentian-dev",
		OpenbaoNamespace:  "openbao",
		RoutingMode:       RoutingModeGateway,
		KubeAPIServerCIDR: "10.96.0.1/32",
	}
}

func tenantLabels(tenantName string) map[string]string {
	return map[string]string{
		TenantLabel:    tenantName,
		ManagedByLabel: ManagedByValue,
	}
}

// Namespace returns the desired tenant Namespace object.
func Namespace(tenantName, nsName string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nsName,
			Labels: tenantLabels(tenantName),
		},
	}
}

// LimitRange returns per-container defaults matching tenant-default Composition.
func LimitRange(tenantName, nsName string) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-limits",
			Namespace: nsName,
			Labels:    tenantLabels(tenantName),
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Default: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					DefaultRequest: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Max: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("8Gi"),
					},
				},
			},
		},
	}
}

// ResourceQuota returns a ResourceQuota when quotas are set; nil otherwise.
func ResourceQuota(tenantName, nsName string, quotas *gentianov1alpha1.TenantQuotas) *corev1.ResourceQuota {
	hard := resourceListFromQuotas(quotas)
	if len(hard) == 0 {
		return nil
	}
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-quota",
			Namespace: nsName,
			Labels:    tenantLabels(tenantName),
		},
		Spec: corev1.ResourceQuotaSpec{Hard: hard},
	}
}

func resourceListFromQuotas(q *gentianov1alpha1.TenantQuotas) corev1.ResourceList {
	if q == nil {
		return nil
	}
	rl := corev1.ResourceList{}
	if q.Storage != nil {
		rl[corev1.ResourceRequestsStorage] = *q.Storage
	}
	if q.CPU != nil {
		rl[corev1.ResourceLimitsCPU] = *q.CPU
	}
	if q.Memory != nil {
		rl[corev1.ResourceLimitsMemory] = *q.Memory
	}
	return rl
}

// NetworkPolicy returns tenant isolation policy aligned with the operator's
// historical rules (pre-C1 owner transfer to Crossplane).
func NetworkPolicy(tenantName, nsName string, cfg Config, kubeAPIEndpts *discoveryv1.EndpointSlice) *networkingv1.NetworkPolicy {
	protocolTCP := corev1.ProtocolTCP
	protocolUDP := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	apiServerPort := intstr.FromInt32(443)

	egressRules := []networkingv1.NetworkPolicyEgressRule{
		namespaceEgress(cfg.InfraNamespace),
		namespaceEgress(KernelNamespace),
		namespaceEgress(cfg.ServicesNamespace),
		namespaceEgress(cfg.OpenbaoNamespace),
		{
			To: []networkingv1.NetworkPolicyPeer{
				{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{TenantLabel: tenantName},
				}},
			},
		},
		namespaceEgress(IngressNamespace),
	}
	if cfg.RoutingMode == RoutingModeGateway {
		egressRules = append(egressRules, namespaceEgress(EnvoyGatewayInstallNamespace))
	}

	egressRules = append(egressRules,
		networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &protocolUDP, Port: &dnsPort},
				{Protocol: &protocolTCP, Port: &dnsPort},
			},
		},
	)
	if cfg.KubeAPIServerCIDR != "" {
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: cfg.KubeAPIServerCIDR}},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &protocolTCP, Port: &apiServerPort},
			},
		})
	}
	if kubeAPIEndpts != nil {
		for _, ep := range kubeAPIEndpts.Endpoints {
			for _, addr := range ep.Addresses {
				for _, port := range kubeAPIEndpts.Ports {
					if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil {
						continue
					}
					endpointPort := intstr.FromInt32(*port.Port)
					egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
						To: []networkingv1.NetworkPolicyPeer{
							{IPBlock: &networkingv1.IPBlock{CIDR: addr + "/32"}},
						},
						Ports: []networkingv1.NetworkPolicyPort{
							{Protocol: &protocolTCP, Port: &endpointPort},
						},
					})
				}
			}
		}
	}

	ingress := []networkingv1.NetworkPolicyIngressRule{
		{From: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{TenantLabel: tenantName}}},
		}},
		namespaceIngress(KernelNamespace),
		namespaceIngress(cfg.InfraNamespace),
		namespaceIngress(cfg.ServicesNamespace),
		namespaceIngress(IngressNamespace),
	}
	if cfg.RoutingMode == RoutingModeGateway {
		ingress = append(ingress, namespaceIngress(EnvoyGatewayInstallNamespace))
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-isolation",
			Namespace: nsName,
			Labels:    tenantLabels(tenantName),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingress,
			Egress:  egressRules,
		},
	}
}

func namespaceEgress(ns string) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
			}},
		},
	}
}

func namespaceIngress(ns string) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
			}},
		},
	}
}

// NamespaceName resolves the tenant namespace from a Tenant spec.
func NamespaceName(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.Namespace != "" {
		return tenant.Spec.Isolation.Namespace
	}
	return fmt.Sprintf("tenant-%s", tenant.Name)
}
