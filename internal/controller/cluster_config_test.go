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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
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

// The model gateway's console is routed only where the claim switches it on,
// and only on a cluster that serves models. Nothing else may answer for the
// claim: a ConfigMap that does not carry the key -- every cluster's, before
// the key existed -- means no console, and there is no environment variable
// that says otherwise.
func TestTheModelGatewayConsoleIsOffUnlessTheClaimSwitchesItOn(t *testing.T) {
	ctx := context.Background()
	t.Setenv("LLM_SUPPORT", "true")
	cases := map[string]struct {
		data map[string]string
		want bool
	}{
		"no ConfigMap":                         {nil, false},
		"a ConfigMap written before the key":   {map[string]string{clusterConfigLLMKey: "true"}, false},
		"the claim says off":                   {map[string]string{clusterConfigLLMKey: "true", clusterConfigLLMConsoleKey: "false"}, false},
		"the claim says on":                    {map[string]string{clusterConfigLLMKey: "true", clusterConfigLLMConsoleKey: "true"}, true},
		"on, on a cluster that serves nothing": {map[string]string{clusterConfigLLMKey: "false", clusterConfigLLMConsoleKey: "true"}, false},
	}
	for name, c := range cases {
		var objs []client.Object
		if c.data != nil {
			objs = append(objs, clusterConfigWith(c.data))
		}
		if got := clusterLLMConsoleRouted(ctx, fakeClusterConfigClient(t, objs...)); got != c.want {
			t.Errorf("%s: console routed = %v, want %v", name, got, c.want)
		}
	}
	if clusterLLMConsoleRouted(ctx, nil) {
		t.Error("no client to ask the claim with, and the console is routed")
	}
}

// The claim's secretMode reaches the Seeder through the ConfigMap. A cluster
// whose ConfigMap does not say reads as derived, which is what it was doing.
func TestClusterSecretModeFollowsTheClaim(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		objs []client.Object
		want secrets.Mode
	}{
		"random":           {[]client.Object{clusterConfigWith(map[string]string{clusterConfigSecretModeKey: "random"})}, secrets.ModeRandom},
		"derived":          {[]client.Object{clusterConfigWith(map[string]string{clusterConfigSecretModeKey: "derived"})}, secrets.ModeDerived},
		"key not written":  {[]client.Object{clusterConfigWith(map[string]string{clusterConfigTenancyKey: "multi"})}, secrets.ModeDerived},
		"no ConfigMap yet": {nil, secrets.ModeDerived},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ClusterSecretMode(fakeClusterConfigClient(t, tc.objs...))(ctx)
			if err != nil || got != tc.want {
				t.Fatalf("mode = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// unreadable fails every read, as an API server does that is away.
type unreadable struct{ client.Reader }

func (unreadable) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("api server away")
}

// What cannot be known is not guessed: an unreadable ConfigMap and a value
// that is neither mode are errors, so nothing is made until the answer is in.
func TestClusterSecretModeDoesNotGuess(t *testing.T) {
	ctx := context.Background()
	if got, err := ClusterSecretMode(unreadable{})(ctx); err == nil {
		t.Errorf("an unreadable ConfigMap read as %q", got)
	}
	if got, err := ClusterSecretMode(nil)(ctx); err == nil {
		t.Errorf("no client read as %q", got)
	}
	c := fakeClusterConfigClient(t, clusterConfigWith(map[string]string{clusterConfigSecretModeKey: "Random"}))
	if got, err := ClusterSecretMode(c)(ctx); err == nil {
		t.Errorf("a misspelt mode read as %q", got)
	}
}

// One answer serves the passes that follow it closely, and a failed read is
// asked again rather than remembered.
func TestClusterSecretModeIsReadOncePerInterval(t *testing.T) {
	ctx := context.Background()
	cm := clusterConfigWith(map[string]string{clusterConfigSecretModeKey: "random"})
	c := fakeClusterConfigClient(t, cm)
	mode := ClusterSecretMode(c)
	if got, err := mode(ctx); err != nil || got != secrets.ModeRandom {
		t.Fatalf("first read: %q, %v", got, err)
	}
	if err := c.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if got, err := mode(ctx); err != nil || got != secrets.ModeRandom {
		t.Fatalf("within the interval: %q, %v; want the answer already held", got, err)
	}
}
