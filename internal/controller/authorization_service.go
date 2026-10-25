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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// The authorization service (requires.services.authorization): a component
// that declares it is given a store of its own, with its own model, at the
// arbiter, and a key good for that store and nothing else. The key is
// written here, once, into the component's Secret; the arbiter's table
// carries its hash and nothing else of it -- as the rights check's key
// (rights_check.go), and for the same reasons.
const (
	// authorizationHolderLabel marks the Secret a component's key is
	// delivered in. The writer of the arbiter's table lists by it.
	authorizationHolderLabel = "gentianos.io/authorization-holder"
	authorizationURLKey      = "AUTHORIZATION_URL"
	authorizationKeyKey      = "AUTHORIZATION_KEY" //nolint:gosec // Secret key name, not a credential.

	reasonAuthorizationModelInvalid = "AuthorizationModelInvalid"
)

func authorizationSecretName(component string) string { return "authorization-" + component }

// wantsAuthorization reports whether the component is given a store.
func wantsAuthorization(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Authorization != nil
}

// arbiterPodLabels are the arbiter's pods' labels, which the way to it from a
// component and the arbiter's own ingress select on.
func arbiterPodLabels() map[string]string { return netpolicy.ArbiterPodLabels() }

func authorizationURL() string {
	return fmt.Sprintf("http://%s.%s.svc:%d", arbiter.ServiceName, arbiter.Namespace, arbiter.Port)
}

// authorizationModelRefusal is why the profile's model cannot be a store's,
// "" when it can or when the profile declares no store. A component whose
// model does not compile is not installed: a store without a model, or with
// the last one that compiled, is a store that decides something its profile
// does not say.
func authorizationModelRefusal(profile *gentianov1alpha1.ComponentProfile) string {
	if !wantsAuthorization(profile) {
		return ""
	}
	if _, _, err := arbiter.Requirement(profile.Services().Authorization); err != nil {
		return "requires.services.authorization: " + err.Error()
	}
	return ""
}

// ensureAuthorization keeps the Secret a component's key is delivered in.
//
// The key is made once and kept: a component reads it at start, and a key
// that changed on every reconcile would be refused until its pods restarted.
//
// A component whose profile does not declare the requirement is not looked
// at. If it declared it once, its Secret stays until the component goes, and
// is worth nothing meanwhile: the arbiter's table lists a key only while the
// profile declares the requirement (authorizationStores).
func (r *ComponentReconciler) ensureAuthorization(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
	if !wantsAuthorization(profile) {
		return nil
	}
	key := types.NamespacedName{Name: authorizationSecretName(comp.Name), Namespace: comp.Namespace}
	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	found := err == nil
	url := authorizationURL()
	if found {
		if !metav1.IsControlledBy(existing, comp) {
			return fmt.Errorf("secret %s exists and is not this component's", key.Name)
		}
		if len(existing.Data[authorizationKeyKey]) > 0 && string(existing.Data[authorizationURLKey]) == url &&
			existing.Labels[authorizationHolderLabel] == "true" {
			return nil
		}
	}
	secret := string(existing.Data[authorizationKeyKey])
	if secret == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		secret = base64.RawURLEncoding.EncodeToString(raw)
	}
	labels := componentLabels(comp)
	labels[authorizationHolderLabel] = "true"
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			authorizationURLKey: []byte(url),
			authorizationKeyKey: []byte(secret),
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

// authorizationEgress is the way to the arbiter: its pods, on its one port,
// and nothing else of the namespace -- not the app instance of OpenFGA
// beside it, whose credential opens every store.
func authorizationEgress() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(arbiter.Port)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": arbiter.Namespace},
			},
			PodSelector: &metav1.LabelSelector{MatchLabels: arbiterPodLabels()},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}

// authorizationStores are the entries the arbiter's table must hold: one
// per component that declares the requirement now and holds its key.
//
// The label finds the candidates and decides nothing, as for the rights
// check's keys (componentKeys). An entry is written only for a Secret in a
// tenant's namespace that a Component of that namespace controls, and whose
// profile declares the requirement now, with a model that compiles. The
// tenant is the namespace's, never something the Secret says about itself.
func authorizationStores(ctx context.Context, c client.Reader) ([]arbiter.Entry, error) {
	list := &corev1.SecretList{}
	if err := c.List(ctx, list, client.MatchingLabels{authorizationHolderLabel: "true"}); err != nil {
		return nil, err
	}
	var out []arbiter.Entry
	for i := range list.Items {
		secret := &list.Items[i]
		tenant, ok := strings.CutPrefix(secret.Namespace, "tenant-")
		owner := metav1.GetControllerOf(secret)
		key := secret.Data[authorizationKeyKey]
		if !ok || tenant == "" || secret.DeletionTimestamp != nil || len(key) == 0 ||
			owner == nil || owner.Kind != "Component" || secret.Name != authorizationSecretName(owner.Name) {
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
		profile, err := getProfileFor(ctx, c, comp.Spec.ProfileRef.Name, comp.Spec.ProfileRef.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if !wantsAuthorization(profile) {
			continue
		}
		requirement := profile.Services().Authorization
		model, tuples, err := arbiter.Requirement(requirement)
		if err != nil {
			// The Component is held for it (authorizationModelRefusal); the
			// store keeps the model it was last handed until one compiles.
			logf.FromContext(ctx).Info("an authorization model does not compile; its store is left out of the table",
				"component", comp.Name, "namespace", comp.Namespace, "error", err.Error())
			continue
		}
		realm, err := realmOfTenantNamespace(ctx, c, secret.Namespace, tenant)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(key)
		out = append(out, arbiter.Entry{
			Tenant: tenant, Component: comp.Name, KeyHash: hex.EncodeToString(sum[:]), Model: model,
			Realm: realm, MirrorGroups: requirement.MirrorsGroups(), Tuples: tuples,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Component < out[j].Component
	})
	return out, nil
}

// realmOfTenantNamespace is the realm of the tenant whose namespace this
// is: the one realm whose memberships reach that tenant's stores.
func realmOfTenantNamespace(ctx context.Context, c client.Reader, namespace, fallback string) (string, error) {
	list := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, list); err != nil {
		return "", err
	}
	for i := range list.Items {
		if list.Items[i].NamespaceName() == namespace {
			return tenantRealmName(&list.Items[i]), nil
		}
	}
	return fallback, nil
}

// AuthorizationTableReconciler writes the arbiter's table: which key opens
// which store, and the model each store is decided against.
type AuthorizationTableReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it reads. Nil holds nothing.
	Definitions *crdcheck.Holder
	client.Client
}

var authorizationTableRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: arbiter.TableConfigMap}}

func (r *AuthorizationTableReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	// Nowhere to write until the namespace is composed, which is when the
	// first component declares the service; that component's arrival brings
	// this back.
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: arbiter.Namespace}, ns); err != nil {
		return ctrl.Result{RequeueAfter: componentRequeue}, client.IgnoreNotFound(err)
	}
	stores, err := authorizationStores(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if stores == nil {
		stores = []arbiter.Entry{}
	}
	body, err := json.Marshal(arbiter.Table{Stores: stores})
	if err != nil {
		return ctrl.Result{}, err
	}
	data := map[string]string{arbiter.TableKey: string(body)}
	key := types.NamespacedName{Name: arbiter.TableConfigMap, Namespace: arbiter.Namespace}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, key, existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{Data: data}
		cm.Name, cm.Namespace = key.Name, key.Namespace
		cm.Labels = map[string]string{managedByLabel: managedByValue}
		return ctrl.Result{}, r.Create(ctx, cm)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(existing.Data, data) {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = data
	return ctrl.Result{}, r.Patch(ctx, existing, patch)
}

func (r *AuthorizationTableReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toTable := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{authorizationTableRequest}
	})
	holders := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[authorizationHolderLabel] == "true"
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("authorization-table").
		Watches(&gentianov1alpha1.Component{}, toTable).
		Watches(&gentianov1alpha1.ComponentProfile{}, toTable).
		Watches(&gentianov1alpha1.Tenant{}, toTable).
		Watches(&corev1.Secret{}, toTable, builder.WithPredicates(holders)).
		Watches(&corev1.Namespace{}, toTable, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetName() == arbiter.Namespace
		}))).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.Component{}, r))
}
