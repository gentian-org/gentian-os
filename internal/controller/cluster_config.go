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

package controller

import (
	"context"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gentian-cluster-config is composed from the Cluster claim by cluster-default,
// and the claim is its only writer. That makes it the operator's window onto
// the claim: reading it here means the operator and the Compositions decide
// from the same source instead of from a Helm value that has to be kept in
// agreement by hand.
const (
	clusterConfigNamespace = "crossplane-system"
	clusterConfigName      = "gentian-cluster-config"
	clusterConfigLLMKey    = "llm.enabled"
)

// clusterLLMEnabled reports whether this cluster serves LLM.
//
// The claim decides, via gentian-cluster-config. The LLM_SUPPORT env var (the
// chart's llmSupport value) is only a fallback for when the ConfigMap cannot
// answer — first boot before the Cluster composition has produced it, a
// ConfigMap predating the key, or a read error. Falling back rather than
// failing keeps the pre-ConfigMap behaviour on exactly the clusters that
// still have it.
func clusterLLMEnabled(ctx context.Context, c client.Reader) bool {
	if c != nil {
		cm := &corev1.ConfigMap{}
		key := types.NamespacedName{Namespace: clusterConfigNamespace, Name: clusterConfigName}
		if err := c.Get(ctx, key, cm); err == nil {
			if v, ok := cm.Data[clusterConfigLLMKey]; ok {
				return v == "true"
			}
		}
	}
	return os.Getenv("LLM_SUPPORT") == "true"
}
