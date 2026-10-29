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
	"reflect"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The log destination inside the cluster (requires.services.logDestination).
//
// The platform's log forwarder runs in the observability namespace and sends
// over HTTPS only. For a destination in the cluster to be one, two things
// have to be true that no tenant can arrange for itself: its Service's name
// carries a certificate the forwarder trusts, and the forwarder's pods may
// reach it. Both are done here, for the one component that declares it.
//
// The certificate is asked of the issuer gentian-ca-platform-services, whose
// role signs names under the platform tenant's namespace and nothing else,
// for servers only. It is asked in the observability namespace, where
// the admission policy lets the platform ask the internal CA and no tenant
// may (gentian-restrict-internal-ca), and its Secret there is what the
// forwarder trusts (ca.crt). A copy of it in the component's namespace is
// what the component serves with. The copy is refreshed on every pass, which
// the manager's periodic resync makes at least every ten hours; the
// certificate is renewed thirty days before it ends.
//
// The network rule admits the forwarder's pods to that one port of the
// component's pods, and nothing else of the kernel.

const (
	// logDestinationCertificate is the Certificate and its Secret in the
	// observability namespace. One per cluster: there is one forwarder.
	logDestinationCertificate = "log-destination-tls"
	// logDestinationIssuer signs the platform tenant's Service names.
	logDestinationIssuer = "gentian-ca-platform-services"
	// ConditionLogDestination says whether the component is the cluster's
	// log destination, and if not why.
	ConditionLogDestination = "LogDestination"

	logDestinationNamespaceLabel = "gentianos.io/log-destination-namespace"
	logDestinationComponentLabel = "gentianos.io/log-destination-component"
)

// forwarderPods selects the log forwarder's pods in the observability
// namespace, by the label the Vector chart puts on them.
var forwarderPods = map[string]string{"app.kubernetes.io/name": "vector"}

func logDestinationSecretName(component string) string { return "log-destination-tls-" + component }

func logDestinationPolicyName(comp *gentianov1alpha1.Component) string {
	return "log-destination-" + comp.Name
}

// logDestinationOf is the declaration a component is held to, or nil where
// it is not honored: outside platform trust, or outside the platform tenant.
func logDestinationOf(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.LogDestinationRequirement {
	services := profile.Services()
	if services == nil || services.LogDestination == nil ||
		profile.Spec.TrustTier != gentianov1alpha1.TrustTierPlatform ||
		comp.Namespace != layout.Tenant(authz.PlatformTenant) {
		return nil
	}
	return services.LogDestination
}

// logDestinationNames are the names the certificate carries: the Service's,
// short and fully qualified.
func logDestinationNames(service, namespace string) []string {
	return []string{
		service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local",
	}
}

func buildLogDestinationCertificate(comp *gentianov1alpha1.Component, ld *gentianov1alpha1.LogDestinationRequirement) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certManagerCertGVK)
	obj.SetName(logDestinationCertificate)
	obj.SetNamespace(layout.Kernel(layout.Observability))
	obj.SetLabels(map[string]string{
		managedByLabel:               managedByValue,
		logDestinationNamespaceLabel: comp.Namespace,
		logDestinationComponentLabel: comp.Name,
	})
	names := make([]interface{}, 0, 2)
	for _, n := range logDestinationNames(ld.Service, comp.Namespace) {
		names = append(names, n)
	}
	obj.Object["spec"] = map[string]interface{}{
		"secretName": logDestinationCertificate,
		"commonName": names[0],
		"dnsNames":   names,
		// The role signs EC P-256 keys for servers only.
		"privateKey": map[string]interface{}{"algorithm": "ECDSA", "size": int64(256), "rotationPolicy": "Always"},
		"usages":     []interface{}{"digital signature", "server auth"},
		"issuerRef":  map[string]interface{}{"name": logDestinationIssuer, "kind": "ClusterIssuer"},
	}
	return obj
}

func buildLogDestinationPolicy(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, port int32) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	p := intstr.FromInt32(port)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      logDestinationPolicyName(comp),
			Namespace: comp.Namespace,
			Labels:    componentLabels(comp),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: componentPods(comp, profile)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": layout.Kernel(layout.Observability)},
					},
					PodSelector: &metav1.LabelSelector{MatchLabels: forwarderPods},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &p}},
			}},
		},
	}
}

// ensureLogDestination does what the declaration asks, or takes away what an
// earlier one was given.
func (r *ComponentReconciler) ensureLogDestination(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
	ld := logDestinationOf(comp, profile)
	if ld == nil {
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, ConditionLogDestination)
		return r.deleteLogDestination(ctx, comp)
	}
	held, err := r.logDestinationHeldElsewhere(ctx, comp)
	if err != nil {
		return err
	}
	if held != "" {
		r.setLogDestinationCondition(comp, metav1.ConditionFalse, "AnotherDestination", fmt.Sprintf(
			"%s is this cluster's log destination, and a cluster has one: this component is given no certificate and the forwarder no way to it",
			held))
		return r.deleteLogDestinationOwn(ctx, comp)
	}
	if err := r.applyLogDestinationCertificate(ctx, comp, ld); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	if err := r.applyLogDestinationPolicy(ctx, comp, profile, ld.Port); err != nil {
		return fmt.Errorf("network policy: %w", err)
	}
	copied, err := r.copyLogDestinationSecret(ctx, comp)
	if err != nil {
		return fmt.Errorf("certificate copy: %w", err)
	}
	if !copied {
		r.setLogDestinationCondition(comp, metav1.ConditionFalse, "Issuing",
			"the internal CA has not yet issued the certificate for "+ld.Service)
		return nil
	}
	r.setLogDestinationCondition(comp, metav1.ConditionTrue, "Issued", fmt.Sprintf(
		"https://%s:%d receives the log forwarder; its certificate is in Secret %s",
		logDestinationNames(ld.Service, comp.Namespace)[0], ld.Port, logDestinationSecretName(comp.Name)))
	return nil
}

func (r *ComponentReconciler) setLogDestinationCondition(comp *gentianov1alpha1.Component, st metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&comp.Status.Conditions, metav1.Condition{
		Type: ConditionLogDestination, Status: st, Reason: reason, Message: message,
		ObservedGeneration: comp.Generation,
	})
}

// logDestinationHeldElsewhere names the component the cluster's certificate
// is for, when that is another one that still is the destination; empty
// when it is this one's, nobody's, or of a component that has gone.
func (r *ComponentReconciler) logDestinationHeldElsewhere(ctx context.Context, comp *gentianov1alpha1.Component) (string, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerCertGVK)
	err := r.Get(ctx, types.NamespacedName{Name: logDestinationCertificate, Namespace: layout.Kernel(layout.Observability)}, cert)
	if errors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ns, name := cert.GetLabels()[logDestinationNamespaceLabel], cert.GetLabels()[logDestinationComponentLabel]
	if ns == comp.Namespace && name == comp.Name {
		return "", nil
	}
	other := &gentianov1alpha1.Component{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, other); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if !other.DeletionTimestamp.IsZero() {
		return "", nil
	}
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: other.Spec.ProfileRef.Name}, profile); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if logDestinationOf(other, profile) == nil {
		return "", nil
	}
	return ns + "/" + name, nil
}

func (r *ComponentReconciler) applyLogDestinationCertificate(ctx context.Context, comp *gentianov1alpha1.Component, ld *gentianov1alpha1.LogDestinationRequirement) error {
	desired := buildLogDestinationCertificate(comp, ld)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certManagerCertGVK)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"]) &&
		reflect.DeepEqual(existing.GetLabels(), desired.GetLabels()) {
		return nil
	}
	existing.Object["spec"] = desired.Object["spec"]
	existing.SetLabels(desired.GetLabels())
	return r.Update(ctx, existing)
}

func (r *ComponentReconciler) applyLogDestinationPolicy(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, port int32) error {
	desired := buildLogDestinationPolicy(comp, profile, port)
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return err
	}
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return nil
	}
	existing.Spec = desired.Spec
	return r.Update(ctx, existing)
}

// copyLogDestinationSecret puts the issued certificate where the component
// mounts it, and says whether there was one to copy.
func (r *ComponentReconciler) copyLogDestinationSecret(ctx context.Context, comp *gentianov1alpha1.Component) (bool, error) {
	issued := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: logDestinationCertificate, Namespace: layout.Kernel(layout.Observability)}, issued)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(issued.Data[corev1.TLSCertKey]) == 0 || len(issued.Data[corev1.TLSPrivateKeyKey]) == 0 {
		return false, nil
	}
	data := map[string][]byte{
		corev1.TLSCertKey:       issued.Data[corev1.TLSCertKey],
		corev1.TLSPrivateKeyKey: issued.Data[corev1.TLSPrivateKeyKey],
		"ca.crt":                issued.Data["ca.crt"],
	}
	key := types.NamespacedName{Name: logDestinationSecretName(comp.Name), Namespace: comp.Namespace}
	existing := &corev1.Secret{}
	err = r.Get(ctx, key, existing)
	if errors.IsNotFound(err) {
		desired := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: componentLabels(comp)},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
		if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
			return false, err
		}
		return true, r.Create(ctx, desired)
	}
	if err != nil {
		return false, err
	}
	if !metav1.IsControlledBy(existing, comp) {
		return false, fmt.Errorf("secret %s exists and is not this component's", key.Name)
	}
	if reflect.DeepEqual(existing.Data, data) {
		return true, nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = data
	return true, r.Patch(ctx, existing, patch)
}

// deleteLogDestination takes away what the component was given: the
// cluster's certificate where it is this component's, and the copy and the
// network rule in its namespace.
func (r *ComponentReconciler) deleteLogDestination(ctx context.Context, comp *gentianov1alpha1.Component) error {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerCertGVK)
	err := r.Get(ctx, types.NamespacedName{Name: logDestinationCertificate, Namespace: layout.Kernel(layout.Observability)}, cert)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if err == nil && cert.GetLabels()[logDestinationNamespaceLabel] == comp.Namespace &&
		cert.GetLabels()[logDestinationComponentLabel] == comp.Name {
		if err := r.Delete(ctx, cert); client.IgnoreNotFound(err) != nil {
			return err
		}
		issued := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: logDestinationCertificate, Namespace: layout.Kernel(layout.Observability)}}
		if err := r.Delete(ctx, issued); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return r.deleteLogDestinationOwn(ctx, comp)
}

// deleteLogDestinationOwn removes the copy and the network rule in the
// component's own namespace.
func (r *ComponentReconciler) deleteLogDestinationOwn(ctx context.Context, comp *gentianov1alpha1.Component) error {
	for _, obj := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: logDestinationSecretName(comp.Name), Namespace: comp.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: logDestinationPolicyName(comp), Namespace: comp.Namespace}},
	} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, comp) {
			continue
		}
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
