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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TURN as a requirement (requires.services.turn): a component that declares
// it, or that activates an addon declaring it, is given the edge's address
// and the shared secret it mints its clients' credentials with, in the Secret
// turn-credentials-<component> of its namespace. A chart reads it by that
// name. The keys name no server software (TURNRequirement).
//
// An addon runs inside the component it activates into, so the Secret is
// that component's: the addon's own Component deploys nothing that could
// read it.
const (
	turnServerKey  = "TURN_SERVER"
	turnPortKey    = "TURN_PORT"
	turnsPortKey   = "TURNS_PORT"
	turnURIsKey    = "TURN_URIS"
	stunURIKey     = "STUN_URI"
	turnSecretKey  = "TURN_SECRET" //nolint:gosec // Secret key name, not a credential.
	turnCredsLabel = "gentianos.io/turn-credentials"
)

func turnCredentialsSecretName(component string) string { return "turn-credentials-" + component }

// declaresTURN reports whether the component or an addon it activates
// declares the requirement. An addon's own Component is never served: it
// deploys nothing that could read the Secret, and its base is served instead.
func declaresTURN(profile *gentianov1alpha1.ComponentProfile, addons []*gentianov1alpha1.ComponentProfile) bool {
	if profile.Spec.Package.Addon != nil {
		return false
	}
	if wantsTURN(profile) {
		return true
	}
	for _, addon := range addons {
		if wantsTURN(addon) {
			return true
		}
	}
	return false
}

// turnCredentials are the values a declaring component is given.
func turnCredentials(kernelDomain, secret string) map[string][]byte {
	host := turnEdgeHost(kernelDomain)
	turn, turns := strconv.Itoa(int(turnPort)), strconv.Itoa(int(turnsPort))
	uris := []string{
		fmt.Sprintf("turn:%s:%s?transport=udp", host, turn),
		fmt.Sprintf("turn:%s:%s?transport=tcp", host, turn),
		fmt.Sprintf("turns:%s:%s?transport=tcp", host, turns),
	}
	return map[string][]byte{
		turnServerKey: []byte(host),
		turnPortKey:   []byte(turn),
		turnsPortKey:  []byte(turns),
		turnURIsKey:   []byte(strings.Join(uris, ",")),
		stunURIKey:    []byte(fmt.Sprintf("stun:%s:%s", host, turn)),
		turnSecretKey: []byte(secret),
	}
}

// ensureTURNCredentials keeps the component's Secret while it declares the
// requirement, and takes it away once it no longer does: the secret opens
// the cluster's relay, and a component that has stopped relaying has no use
// for it.
//
// It is asked only after the engine answered (systemEngineWaiting), so the
// edge exists and its secret is in the vault.
func (r *ComponentReconciler) ensureTURNCredentials(
	ctx context.Context, comp *gentianov1alpha1.Component,
	profile *gentianov1alpha1.ComponentProfile, addons []*gentianov1alpha1.ComponentProfile,
) error {
	key := types.NamespacedName{Name: turnCredentialsSecretName(comp.Name), Namespace: comp.Namespace}
	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	found := err == nil
	if found && !metav1.IsControlledBy(existing, comp) {
		return fmt.Errorf("secret %s exists and is not this component's", key.Name)
	}
	if !declaresTURN(profile, addons) {
		if found {
			return client.IgnoreNotFound(r.Delete(ctx, existing))
		}
		return nil
	}
	secret, err := turnSharedSecret(ctx, r.Seeder)
	if err != nil {
		return err
	}
	labels := componentLabels(comp)
	labels[turnCredsLabel] = "true"
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       turnCredentials(r.KernelDomain, secret),
	}
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return err
	}
	if !found {
		return r.Create(ctx, desired)
	}
	same := len(existing.Data) == len(desired.Data) && existing.Labels[turnCredsLabel] == "true"
	for k, v := range desired.Data {
		same = same && string(existing.Data[k]) == string(v)
	}
	if same {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	return r.Patch(ctx, existing, patch)
}
