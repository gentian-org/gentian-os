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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A partner app reads its client, token endpoint and audience from the
// Secret partners-<profile> as environment variables, and the tenant's
// composition writes that Secret some time after the app's release exists.
// An environment is read once, at a container's start: a pod started before
// the Secret arrived runs without it until something restarts it, and so
// does one started before the Secret changed. So the operator stamps a
// checksum of the Secret's data on the app's pod templates; a Secret that
// appears or changes changes the stamp, and the workload rolls.

// partnersChecksumAnnotation is the stamp on a partner app's pod templates.
const partnersChecksumAnnotation = "gentianos.io/partners-checksum"

// partnersChecksum is the SHA-256 of a Secret's data, keys sorted.
func partnersChecksum(secret *corev1.Secret) string {
	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s=%d:", k, len(secret.Data[k]))
		h.Write(secret.Data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rollOnPartnersSecret stamps the checksum of the component's partners
// Secret on the pod template of every Deployment and StatefulSet of the
// component whose stamp differs. A Secret that does not exist yet stamps
// nothing: its arrival is what rolls the pods.
func (r *ComponentReconciler) rollOnPartnersSecret(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: PartnerSecretName(profile.Name), Namespace: comp.Namespace}, secret)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sum := partnersChecksum(secret)
	stamp := func(meta *metav1.ObjectMeta) bool {
		if meta.Annotations[partnersChecksumAnnotation] == sum {
			return false
		}
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		meta.Annotations[partnersChecksumAnnotation] = sum
		return true
	}

	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(comp.Namespace)); err != nil {
		return err
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		if !workloadOfApp(d.ObjectMeta, d.Spec.Template, comp.Name) {
			continue
		}
		patch := client.MergeFrom(d.DeepCopy())
		if stamp(&d.Spec.Template.ObjectMeta) {
			if err := r.Patch(ctx, d, patch); err != nil {
				return fmt.Errorf("roll %s/%s for its partners Secret: %w", d.Namespace, d.Name, err)
			}
		}
	}
	statefulSets := &appsv1.StatefulSetList{}
	if err := r.List(ctx, statefulSets, client.InNamespace(comp.Namespace)); err != nil {
		return err
	}
	for i := range statefulSets.Items {
		st := &statefulSets.Items[i]
		if !workloadOfApp(st.ObjectMeta, st.Spec.Template, comp.Name) {
			continue
		}
		patch := client.MergeFrom(st.DeepCopy())
		if stamp(&st.Spec.Template.ObjectMeta) {
			if err := r.Patch(ctx, st, patch); err != nil {
				return fmt.Errorf("roll %s/%s for its partners Secret: %w", st.Namespace, st.Name, err)
			}
		}
	}
	return nil
}

// componentsOfPartnersSecret re-runs the Components of a profile when its
// partners Secret appears, changes or goes.
func componentsOfPartnersSecret(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return mapPartnersSecret(ctx, c, obj)
	})
}

func mapPartnersSecret(ctx context.Context, c client.Client, obj client.Object) []reconcile.Request {
	profile, ok := strings.CutPrefix(obj.GetName(), PartnerSecretName(""))
	if !ok || profile == "" {
		return nil
	}
	list := &gentianov1alpha1.ComponentList{}
	if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ProfileRef.Name == profile {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
			}})
		}
	}
	return out
}
