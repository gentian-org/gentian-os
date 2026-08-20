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
	clusterConfigNamespace     = "crossplane-system"
	clusterConfigName          = "gentian-cluster-config"
	clusterConfigLLMKey        = "llm.enabled"
	clusterConfigMailModeKey   = "mail.serviceMode"
	clusterConfigMailEgressKey = "mail.egressHost"
)

// clusterConfigValue returns what the claim says for key, or the named
// environment variable when the ConfigMap cannot answer.
//
// The fallback is for the cases where the ConfigMap legitimately cannot speak
// yet — first boot before the Cluster composition has produced it, a ConfigMap
// predating the key, or a read error — and not a second opinion. Where both
// exist the ConfigMap wins, because it is derived from the claim and the env
// var is a Helm value someone has to remember to keep in step.
//
// Note what a missing key costs: the reader silently falls back and the cluster
// runs on the Helm value, which is how mail.serviceMode came to read kernel in
// the claim and external in the operator. lint-cluster-config-keys.py fails the
// build when a key read here is not written by the composition, so that
// silence cannot be reintroduced.
func clusterConfigValue(ctx context.Context, c client.Reader, key, envVar string) string {
	return clusterConfigValueOr(ctx, c, key, os.Getenv(envVar))
}

// clusterConfigValueOr is the same, for a caller that already holds its
// fallback — a field on the reconciler rather than an environment variable.
// Keeping the fallback a value means the caller decides what "the ConfigMap
// could not answer" falls back to, and stays testable without setting env.
func clusterConfigValueOr(ctx context.Context, c client.Reader, key, fallback string) string {
	if c != nil {
		cm := &corev1.ConfigMap{}
		k := types.NamespacedName{Namespace: clusterConfigNamespace, Name: clusterConfigName}
		if err := c.Get(ctx, k, cm); err == nil {
			if v, ok := cm.Data[key]; ok && v != "" {
				return v
			}
		}
	}
	return fallback
}

// clusterMailServiceMode reports this cluster's mail stack — kernel or external.
//
// From the claim via gentian-cluster-config, with MAIL_SERVICE_MODE as the
// fallback described above. The two disagreed for as long as they were separate:
// the claim said kernel while the operator, reading only its Helm value, said
// external and skipped Dovecot provisioning entirely.
func clusterMailServiceMode(ctx context.Context, c client.Reader, fallback string) string {
	return clusterConfigValueOr(ctx, c, clusterConfigMailModeKey, fallback)
}

// clusterLLMEnabled reports whether this cluster serves LLM.
//
// The claim decides, via gentian-cluster-config. The LLM_SUPPORT env var (the
// chart's llmSupport value) is only a fallback for when the ConfigMap cannot
// answer — first boot before the Cluster composition has produced it, a
// ConfigMap predating the key, or a read error. Falling back rather than
// failing keeps the pre-ConfigMap behaviour on exactly the clusters that
// still have it.
func clusterLLMEnabled(ctx context.Context, c client.Reader) bool {
	return clusterConfigValue(ctx, c, clusterConfigLLMKey, "LLM_SUPPORT") == "true"
}

// clusterMailEgressHost is the name that resolves to the address mail leaves
// from, used to build each tenant's SPF record.
//
// From the claim for the same reason as the mode above. Getting this from a
// second place is not a tidiness question: SPF names the sending address, so a
// stale answer authorises the wrong host and every message soft-fails against a
// record that reads as correct.
func clusterMailEgressHost(ctx context.Context, c client.Reader, fallback string) string {
	return clusterConfigValueOr(ctx, c, clusterConfigMailEgressKey, fallback)
}
