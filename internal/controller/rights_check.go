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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// The rights check (requires.services.rights): a component that declares it
// is given a key of its own for one question at the bouncer -- may this
// person use that app of my tenant -- and the bouncer's table is told the
// key exists. The key is written here, once, into the component's Secret;
// the table carries its hash and nothing else of it.
const (
	// rightsCheckerLabel marks the Secret a component's key is delivered in.
	// The writer of the bouncer's table lists by it.
	rightsCheckerLabel = "gentianos.io/rights-checker"
	// rightsCheckPort is the bouncer's listener for the question
	// (cmd/bouncer, BOUNCER_CHECK_LISTEN: the two must agree).
	rightsCheckPort   = netpolicy.RightsCheckPort
	rightsCheckURLKey = "RIGHTS_CHECK_URL"
	rightsCheckKeyKey = "RIGHTS_CHECK_KEY" //nolint:gosec // Secret key name, not a credential.
)

func rightsCheckSecretName(component string) string { return "rights-check-" + component }

func wantsRightsCheck(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Rights != nil
}

func rightsCheckURL(bouncerService string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d/v1/check", bouncerService, servicesNamespace, rightsCheckPort)
}

// ensureRightsCheck keeps the Secret a component's key is delivered in.
//
// The key is made once and kept: a component reads it at start, and a key
// that changed on every reconcile would be refused until its pods restarted.
//
// A component whose profile does not declare the requirement is not looked
// at. If it declared it once, its Secret stays until the component goes, and
// is worth nothing meanwhile: the bouncer's table lists a key only while the
// profile declares the requirement (rightsCheckers).
func (r *ComponentReconciler) ensureRightsCheck(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
	if !wantsRightsCheck(profile) {
		return nil
	}
	key := types.NamespacedName{Name: rightsCheckSecretName(comp.Name), Namespace: comp.Namespace}
	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	found := err == nil
	url := rightsCheckURL(r.bouncerService())
	if found {
		if !metav1.IsControlledBy(existing, comp) {
			return fmt.Errorf("secret %s exists and is not this component's", key.Name)
		}
		if len(existing.Data[rightsCheckKeyKey]) > 0 && string(existing.Data[rightsCheckURLKey]) == url &&
			existing.Labels[rightsCheckerLabel] == "true" {
			return nil
		}
	}
	secret := string(existing.Data[rightsCheckKeyKey])
	if secret == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		secret = base64.RawURLEncoding.EncodeToString(raw)
	}
	labels := componentLabels(comp)
	labels[rightsCheckerLabel] = "true"
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			rightsCheckURLKey: []byte(url),
			rightsCheckKeyKey: []byte(secret),
		},
	}
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return err
	}
	if !found {
		return r.Create(ctx, desired)
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	return r.Patch(ctx, existing, patch)
}

// rightsCheckEgress is the way to the bouncer's listener for the question:
// the edge namespace on that one port, and nothing else there.
func rightsCheckEgress() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(rightsCheckPort)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": servicesNamespace},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}

// bouncerChecker is one checker of the bouncer's table (internal/bouncer).
type bouncerChecker struct {
	Tenant    string `json:"tenant"`
	Component string `json:"component"`
	KeyHash   string `json:"keyHash"`
}

// rightsCheckers are the checkers the bouncer's table must hold: one per
// component that declares the requirement and holds its key.
//
// The label finds the candidates and decides nothing. An entry is written
// only for a Secret in a tenant's namespace that a Component of that
// namespace controls, and whose profile declares the requirement now. The
// tenant is the namespace's, never something the Secret says about itself.
func rightsCheckers(ctx context.Context, c client.Reader) ([]bouncerChecker, error) {
	list := &corev1.SecretList{}
	if err := c.List(ctx, list, client.MatchingLabels{rightsCheckerLabel: "true"}); err != nil {
		return nil, err
	}
	var out []bouncerChecker
	for i := range list.Items {
		secret := &list.Items[i]
		tenant, ok := strings.CutPrefix(secret.Namespace, "tenant-")
		owner := metav1.GetControllerOf(secret)
		key := secret.Data[rightsCheckKeyKey]
		if !ok || tenant == "" || secret.DeletionTimestamp != nil || len(key) == 0 ||
			owner == nil || owner.Kind != "Component" || secret.Name != rightsCheckSecretName(owner.Name) {
			continue
		}
		comp := &gentianov1alpha1.Component{}
		if err := c.Get(ctx, types.NamespacedName{Name: owner.Name, Namespace: secret.Namespace}, comp); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if comp.UID != owner.UID || comp.DeletionTimestamp != nil {
			continue
		}
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := c.Get(ctx, types.NamespacedName{Name: comp.Spec.ProfileRef.Name}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if !wantsRightsCheck(profile) {
			continue
		}
		sum := sha256.Sum256(key)
		out = append(out, bouncerChecker{Tenant: tenant, Component: comp.Name, KeyHash: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Component < out[j].Component
	})
	return out, nil
}
