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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fakeClusterConfigClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func clusterConfigWith(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterConfigName,
			Namespace: clusterConfigNamespace,
		},
		Data: data,
	}
}

// The claim decides, whatever the env says. This is the whole point of reading
// gentian-cluster-config: a values-file llmSupport that disagrees with the
// claim's llm.enabled must not win.
func TestClusterLLMEnabledConfigMapOverridesEnv(t *testing.T) {
	ctx := context.Background()

	t.Setenv("LLM_SUPPORT", "true")
	c := fakeClusterConfigClient(t, clusterConfigWith(map[string]string{clusterConfigLLMKey: "false"}))
	if clusterLLMEnabled(ctx, c) {
		t.Fatal("llm.enabled=false on the ConfigMap, but env true won")
	}

	t.Setenv("LLM_SUPPORT", "false")
	c = fakeClusterConfigClient(t, clusterConfigWith(map[string]string{clusterConfigLLMKey: "true"}))
	if !clusterLLMEnabled(ctx, c) {
		t.Fatal("llm.enabled=true on the ConfigMap, but env false won")
	}
}

// Without the ConfigMap (first boot, before the Cluster composition has
// produced it) or without the key (a ConfigMap predating it), the env keeps
// the old behaviour.
func TestClusterLLMEnabledFallsBackToEnv(t *testing.T) {
	ctx := context.Background()

	t.Setenv("LLM_SUPPORT", "true")
	if !clusterLLMEnabled(ctx, fakeClusterConfigClient(t)) {
		t.Fatal("no ConfigMap: env true should win")
	}
	if !clusterLLMEnabled(ctx, fakeClusterConfigClient(t, clusterConfigWith(map[string]string{"node.ip": "10.0.0.1"}))) {
		t.Fatal("ConfigMap without llm.enabled: env true should win")
	}

	t.Setenv("LLM_SUPPORT", "false")
	if clusterLLMEnabled(ctx, fakeClusterConfigClient(t)) {
		t.Fatal("no ConfigMap and env false: expected disabled")
	}
	if clusterLLMEnabled(ctx, nil) {
		t.Fatal("nil client and env false: expected disabled")
	}
}
