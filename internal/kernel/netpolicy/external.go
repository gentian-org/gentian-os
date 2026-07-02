/*
Copyright 2026 The Gentian Authors.

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

package netpolicy

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// ExternalAccessNetworkPolicy grants egress from an app workload to external
// destinations on ports declared via gentianos.io/external-egress-ports.
func ExternalAccessNetworkPolicy(
	tenantName, nsName, appName string,
	profile *gentianov1alpha1.AppProfile,
) *networkingv1.NetworkPolicy {
	if profile == nil {
		return nil
	}
	ports := gentianov1alpha1.ProfileExternalEgressPorts(profile)
	if len(ports) == 0 {
		return nil
	}

	protocolTCP := corev1.ProtocolTCP
	npPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, port := range ports {
		p := intstr.FromInt32(port)
		npPorts = append(npPorts, networkingv1.NetworkPolicyPort{
			Protocol: &protocolTCP,
			Port:     &p,
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalPolicyName(appName),
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyExternal),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: appName},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
					},
					Ports: npPorts,
				},
			},
		},
	}
}

func externalPolicyName(appName string) string {
	name := fmt.Sprintf("external-access-%s", appName)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
