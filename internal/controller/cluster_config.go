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
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// gentian-cluster-config is composed from the Cluster claim by cluster-default,
// and the claim is its only writer. That makes it the operator's window onto
// the claim: reading it here means the operator and the Compositions decide
// from the same source instead of from a Helm value that has to be kept in
// agreement by hand.
const (
	clusterConfigName          = "gentian-cluster-config"
	clusterConfigLLMKey        = "llm.enabled"
	clusterConfigLLMConsoleKey = "llm.console.enabled"
	clusterConfigTenancyKey    = "tenancyMode"
	clusterConfigMailModeKey   = "mail.serviceMode"
	clusterConfigMailEgressKey = "mail.egressHost"
	clusterConfigSecretModeKey = "secretMode"
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
// clusterConfigNamespace is where the Cluster composition writes the
// ConfigMap: the provisioning namespace, by function, never by name.
var clusterConfigNamespace = layout.Namespace(layout.Provisioning)

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

// ClusterTenancyMode reports whether this cluster serves one tenant or many.
//
// From the claim, with the operator's own value as the fallback. Every host a
// tenant is reachable at is derived from this, so the claim saying one thing
// while the running operator believes another is not cosmetic: it decides
// whether an app answers on <app>.<tenant>.<kernel> or on <app>.<kernel>.
// It also makes the setting changeable through the director, which writes the
// claim and nothing else.
func ClusterTenancyMode(ctx context.Context, c client.Reader, fallback string) string {
	return clusterConfigValueOr(ctx, c, clusterConfigTenancyKey, fallback)
}

// clusterMailServiceMode reports this cluster's mail stack — kernel or external.
//
// From the claim via gentian-cluster-config, with MAIL_SERVICE_MODE as the
// fallback described above. The two disagreed for as long as they were separate:
// the claim said kernel while the operator, reading only its Helm value, said
// external and skipped Dovecot provisioning entirely.
// mailServiceModeSystem is the cluster running its own mail stack.
//
// It was spelled "kernel", and that was wrong under the layout: Postfix and
// Dovecot live in system-mail and system-mail-dmz, tier system, and no kernel
// namespace is involved. The tier vocabulary already has the word, so this is
// the one that needs no second one.
const mailServiceModeSystem = "system"

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

// clusterLLMConsoleRouted reports whether the model gateway's console is
// served at llm.<kernel domain>: the cluster serves models and its claim
// switches the console on (llm.console.enabled).
//
// Off unless the claim says so, and there is deliberately no environment
// fallback: the gateway is a system service, which has no public route, and
// the console is the one departure from that a cluster may choose. A
// ConfigMap that cannot answer -- not composed yet, or written before the key
// existed -- therefore means no route, never one.
func clusterLLMConsoleRouted(ctx context.Context, c client.Reader) bool {
	return clusterLLMEnabled(ctx, c) && clusterConfigValueOr(ctx, c, clusterConfigLLMConsoleKey, "") == "true"
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

// clusterSecretModeTTL is how long one answer about the secret mode is used.
// A credential is made once and the mode is set at install, so the answer may
// be a little old; it may not cost an API call for every value of every pass.
const clusterSecretModeTTL = 30 * time.Second

// ClusterSecretMode gives the Seeder its window onto the claim's secretMode:
// derived, where a credential nothing has stored yet is computed from the
// master password, or random, where it is drawn anew and the master password
// is not used.
//
// There is no environment fallback, and that is deliberate: this decides for
// good how a credential is made, so a Helm value that disagreed with the claim
// would be a second opinion on exactly the wrong question.
//
// A ConfigMap that is not there yet, or that was written before the key
// existed, reads as derived: the schema's default, and what every cluster did
// before the mode was read here. Any other failure to read, and any value that
// is neither of the two, is an error -- the Seeder then makes nothing and the
// reconcile comes back, which is better than a credential made on a guess.
func ClusterSecretMode(c client.Reader) secrets.ModeFunc {
	var (
		mu     sync.Mutex
		cached secrets.Mode
		readAt time.Time
	)
	return func(ctx context.Context) (secrets.Mode, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != "" && time.Now().Sub(readAt) < clusterSecretModeTTL {
			return cached, nil
		}
		mode, err := readClusterSecretMode(ctx, c)
		if err != nil {
			return "", err
		}
		cached, readAt = mode, time.Now()
		return mode, nil
	}
}

func readClusterSecretMode(ctx context.Context, c client.Reader) (secrets.Mode, error) {
	if c == nil {
		return "", fmt.Errorf("%s cannot be read: no client", clusterConfigName)
	}
	cm := &corev1.ConfigMap{}
	k := types.NamespacedName{Namespace: clusterConfigNamespace, Name: clusterConfigName}
	if err := c.Get(ctx, k, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return secrets.ModeDerived, nil
		}
		return "", fmt.Errorf("read %s/%s: %w", k.Namespace, k.Name, err)
	}
	switch v := cm.Data[clusterConfigSecretModeKey]; secrets.Mode(v) {
	case "", secrets.ModeDerived:
		return secrets.ModeDerived, nil
	case secrets.ModeRandom:
		return secrets.ModeRandom, nil
	default:
		return "", fmt.Errorf("%s/%s: %s is %q, which is neither %q nor %q",
			k.Namespace, k.Name, clusterConfigSecretModeKey, v, secrets.ModeDerived, secrets.ModeRandom)
	}
}
