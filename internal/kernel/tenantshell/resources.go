/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	TenantLabel                  = meta.TenantLabel
	ManagedByLabel               = meta.ManagedByLabel
	ManagedByValue               = meta.ManagedByValue
	KernelNamespace              = meta.KernelNamespace
	OperatorNamespace            = meta.OperatorNamespace
	EnvoyGatewayInstallNamespace = meta.EnvoyGatewayInstallNamespace
	RoutingModeGateway           = meta.RoutingModeGateway
)

// Config is shared with netpolicy for MAC policy generation.
type Config = netpolicy.Config

// DefaultConfig returns test-friendly defaults.
func DefaultConfig() Config {
	return netpolicy.DefaultConfig()
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
	hard := ResourceListFromQuotas(quotas)
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

// ResourceListFromQuotas maps a tenant's plan quantities onto the ResourceQuota
// keys that enforce them.
//
// It is NOT what creates the quota. The tenant-default Composition composes the
// real ResourceQuota through provider-kubernetes, and this mirrors that
// template — the mapping exists twice, in Go and in Go templating, and the
// Composition is the one the cluster obeys.
//
// Two readers depend on the mirror holding. The downgrade guard in
// internal/resourceplan compares a candidate plan against these keys, so a
// mapping that drifts from the Composition would pass a plan against
// requests.cpu and see it enforced against limits.cpu. And
// xtenant_shell_simulator_test.go stands in for the Composition, so a test
// suite built on a stale mirror agrees with itself and with nothing real.
//
// That is not hypothetical: requests.cpu and requests.memory were added here
// and not to the Composition, and a tenant moved onto a plan received its
// limits and none of its reserved capacity. Adding them to the Composition was
// not enough either — the quantities still have to survive the Tenant -> XTenant
// projection in internal/controller and the XRD's structural schema, and they
// survived neither, so the same tenants kept getting the same half a plan.
//
// Change one, change all four: this mirror, the Composition, xtenantQuotas in
// internal/controller, and crossplane/xrds/tenant.yaml. Two tests now fail when
// any of them drifts — composition_agreement_test.go here, and
// xtenant_quotas_agreement_test.go for the two hops between Tenant and XTenant.
func ResourceListFromQuotas(q *gentianov1alpha1.TenantQuotas) corev1.ResourceList {
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
	if q.RequestsCPU != nil {
		rl[corev1.ResourceRequestsCPU] = *q.RequestsCPU
	}
	if q.RequestsMemory != nil {
		rl[corev1.ResourceRequestsMemory] = *q.RequestsMemory
	}
	if q.MaxPods > 0 {
		rl[corev1.ResourcePods] = resource.MustParse(fmt.Sprintf("%d", q.MaxPods))
	}
	return rl
}

// NetworkPolicy returns the default-deny baseline for a tenant namespace.
func NetworkPolicy(tenantName, nsName string, cfg Config, kubeAPIEndpts *discoveryv1.EndpointSlice) *networkingv1.NetworkPolicy {
	return netpolicy.BaselineNetworkPolicy(tenantName, nsName, cfg, kubeAPIEndpts)
}

// NamespaceName resolves the tenant namespace from a Tenant spec.
func NamespaceName(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.Namespace != "" {
		return tenant.Spec.Isolation.Namespace
	}
	return fmt.Sprintf("tenant-%s", tenant.Name)
}
