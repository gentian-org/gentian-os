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
	"encoding/base64"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/director/authz"
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

// wantsRightsCheck reports whether the component is given a key of its own:
// one that declares the rights check, and one that vouches for people, which
// presents the same key where it takes a link away (vouching.go).
func wantsRightsCheck(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && (services.Rights != nil || services.Vouching != nil)
}

// holdsComponentKey reports whether the component is given a key of its own
// at all: one that asks the rights check, one that vouches, and one that
// commissions partners, which presents the key at the director
// (partner_commissioners.go). The key is one per component whatever it is for; what
// it opens is decided where it is presented, by the lists the operator
// writes for each purpose.
func holdsComponentKey(profile *gentianov1alpha1.ComponentProfile) bool {
	return wantsRightsCheck(profile) || wantsPartnerCommission(profile)
}

func rightsCheckURL(bouncerService string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d/v1/check", bouncerService, servicesNamespace, rightsCheckPort)
}

// ensureRightsCheck keeps the Secret a component's own key is delivered in
// (ensureKeySecret), with the bouncer's address and, for a component that
// commissions partners there, the director's.
//
// A component whose profile does not declare the requirement is not looked
// at. If it declared it once, its Secret stays until the component goes, and
// is worth nothing meanwhile: the bouncer's table lists a key only while the
// profile declares the requirement (rightsCheckers).
func (r *ComponentReconciler) ensureRightsCheck(ctx context.Context, comp *gentianov1alpha1.Component, tenantName string, profile *gentianov1alpha1.ComponentProfile) error {
	if !holdsComponentKey(profile) {
		return nil
	}
	// The director's address is absent for a component that does not
	// commission partners.
	director := ""
	if wantsPartnerCommission(profile) {
		director = r.directorURL()
	}
	return r.ensureKeySecret(ctx, comp, tenantName, rightsCheckKeys, map[string]string{
		rightsCheckURLKey:       rightsCheckURL(r.bouncerService()),
		componentDirectorURLKey: director,
	})
}

// ensureKeySecret keeps a Secret of the kind keys describes for the
// component: its key and the addresses it is presented at, each address by
// the entry it is in, an empty one absent.
//
// The key is made once and kept: a component reads it at start, and a key
// that changed on every reconcile would be refused until its pods restarted.
func (r *ComponentReconciler) ensureKeySecret(ctx context.Context, comp *gentianov1alpha1.Component, tenantName string, keys keySecret, addresses map[string]string) error {
	key := types.NamespacedName{Name: keys.name(comp.Name), Namespace: comp.Namespace}
	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	found := err == nil
	if found {
		if !metav1.IsControlledBy(existing, comp) {
			return fmt.Errorf("secret %s exists and is not this component's", key.Name)
		}
		current := len(existing.Data[keys.key]) > 0 && existing.Labels[keys.label] == "true"
		for entry, address := range addresses {
			current = current && string(existing.Data[entry]) == address
		}
		if current {
			return nil
		}
	}
	secret := string(existing.Data[keys.key])
	if secret == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		secret = base64.RawURLEncoding.EncodeToString(raw)
	}
	labels := componentLabels(comp, tenantName)
	labels[keys.label] = "true"
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{keys.key: []byte(secret)},
	}
	for entry, address := range addresses {
		if address != "" {
			desired.Data[entry] = []byte(address)
		}
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

// wantsAuditCheck reports whether a profile asks the audit questions with its
// key (requires.services.rights.audit). The schema holds the rights check to
// platform trust; rightsCheckers holds the audit questions to the platform
// tenant.
func wantsAuditCheck(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Rights != nil && services.Rights.Audit &&
		profile.Spec.TrustTier == gentianov1alpha1.TrustTierPlatform
}

// rightsCheckers are the checkers the bouncer's table must hold: one per
// component that declares the requirement and holds its key. A component of
// the platform tenant whose profile asks the audit questions has them, asked
// against audit; one of any other tenant asks the app question alone,
// whatever its profile says.
func rightsCheckers(ctx context.Context, c client.Reader, audit bouncer.CheckerAudit) ([]bouncer.Checker, error) {
	keys, err := componentKeys(ctx, c, rightsCheckKeys, wantsRightsCheck)
	if err != nil {
		return nil, err
	}
	auditing, err := componentKeys(ctx, c, rightsCheckKeys, wantsAuditCheck)
	if err != nil {
		return nil, err
	}
	audits := map[string]bool{}
	for _, k := range auditing {
		if k.Tenant == authz.PlatformTenant {
			audits[k.KeyHash] = true
		}
	}
	var out []bouncer.Checker
	for _, k := range keys {
		checker := bouncer.Checker{Key: componentkeys.Key{Tenant: k.Tenant, Component: k.Component, KeyHash: k.KeyHash}}
		if audits[k.KeyHash] {
			checker.Audit = &audit
		}
		out = append(out, checker)
	}
	return out, nil
}

// keySecret is a kind of Secret a component's key is delivered in: the
// label that finds them, the entry the key is in, and the Secret's name for a
// component.
type keySecret struct {
	label string
	key   string
	name  func(component string) string
}

// rightsCheckKeys are the keys for the rights check, which the bouncer's
// table, the registrar's list and the director's list are written from.
var rightsCheckKeys = keySecret{label: rightsCheckerLabel, key: rightsCheckKeyKey, name: rightsCheckSecretName}

// componentKey is one component that holds a key of its own, with the hash
// of the key. The key itself never leaves the component's Secret.
type componentKey struct {
	Tenant    string
	Component string
	// Profile is the profile the component runs.
	Profile string
	KeyHash string

	// holder is the component's tenant, profile the profile it runs and
	// namespace its namespace.
	holder    *gentianov1alpha1.Tenant
	profile   *gentianov1alpha1.ComponentProfile
	namespace string
}

// componentKeys are the components that hold a key of the kind and whose
// profile says, now, that they want it for the purpose asked about. It is
// the one reading of those Secrets: every list of keys is written from it,
// each with its own kind and wants.
//
// The label finds the candidates and decides nothing. An entry is written
// only for a Secret in a tenant's namespace that a Component of that
// namespace controls, and whose profile declares the requirement now. The
// tenant is the namespace's, never something the Secret says about itself.
func componentKeys(ctx context.Context, c client.Reader, kind keySecret, wants func(*gentianov1alpha1.ComponentProfile) bool) ([]componentKey, error) {
	list := &corev1.SecretList{}
	if err := c.List(ctx, list, client.MatchingLabels{kind.label: "true"}); err != nil {
		return nil, err
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenants); err != nil {
		return nil, err
	}
	var out []componentKey
	for i := range list.Items {
		secret := &list.Items[i]
		holder := gentianov1alpha1.TenantInNamespace(tenants.Items, secret.Namespace)
		owner := metav1.GetControllerOf(secret)
		key := secret.Data[kind.key]
		if holder == nil || secret.DeletionTimestamp != nil || len(key) == 0 ||
			owner == nil || owner.Kind != "Component" || secret.Name != kind.name(owner.Name) {
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
		if !wants(profile) {
			continue
		}
		out = append(out, componentKey{
			Tenant: holder.Name, Component: comp.Name, Profile: profile.Name, KeyHash: componentkeys.Hash(string(key)),
			holder: holder, profile: profile, namespace: comp.Namespace,
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
