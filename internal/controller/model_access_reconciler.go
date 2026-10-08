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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/modelgateway"
)

// The model gateway as a declared requirement (requires.services.llm).
//
// The gateway used to be handed to every app of a cluster that ran one: each
// got a key and a Secret whether or not it ever called a model, the key was
// the text sk-gentian-<tenant>-<app>, which anybody who knew the two names
// could write down, and no network path was opened, so the one app that did
// call models named the gateway's whole namespace in an annotation.
//
// It is a requirement now, served here like a database or a cache is served
// by its reconciler. An app whose profile declares it gets:
//
//   - a key of its own, generated as a database password is (the vault's
//     seeder: derived from the master for this tenant and app, or random) and
//     kept in the vault beside the app's other credentials;
//   - that key registered at the gateway under the app's alias, replacing
//     whatever was registered there before;
//   - the Secret llm-credentials-<app> in the tenant's namespace, and the
//     same two values through valueMapping.llm where the chart takes them;
//   - a network path to the gateway's port, from the app's kernel-access
//     policy (internal/kernel/netpolicy).
//
// An app that does not declare it gets none of them, and what an earlier
// version gave it is taken away.
//
// A component the platform places on tenants itself -- the desktop, the
// administration console: defaultForTenants and its two siblings -- is
// served the same way when its profile is of platform trust: a key per
// tenant and component under the same alias, vault path and Secret name,
// with the component's name where an app's would be. Three things differ,
// because nothing installs or uninstalls such a component and the operator
// renders its release itself:
//
//   - the key is written on the tenant's record here, before it is
//     registered, and is removed when the tenant is deleted with its data,
//     when the profile stops declaring the gateway, or when the platform
//     takes the component away. A tenant cannot purge it;
//   - the chart is told the gateway's address and the NAME of the Secret
//     (valueMapping.llm.baseUrlKey, secretNameKey, availableKey). The key is
//     never a release value;
//   - the network path is a rule of the component's own policy
//     (component_network_policy.go), opened once the key is delivered.

// modelCredentialsSecretName is the Secret an app's model access is delivered
// in, in the tenant's namespace. Profiles consume it by this name.
func modelCredentialsSecretName(app string) string { return "llm-credentials-" + app }

// The keys of that Secret: the names the OpenAI clients read from their
// environment, so an app can take the Secret whole with envFrom.
const (
	modelCredentialsBaseKey    = "OPENAI_API_BASE"
	modelCredentialsBaseURLKey = "OPENAI_API_BASE_URL"
	modelCredentialsAPIKey     = "OPENAI_API_KEY" //nolint:gosec // Secret key name, not a credential.
)

// modelAccessState is what one pass found, for the log line and the tests.
type modelAccessState struct {
	// served are the apps whose key is registered and delivered.
	served []string
	// waiting are the declaring apps that could not be served this pass,
	// each with why.
	waiting []string
	// removed are the apps whose key and Secret were taken away.
	removed []string
}

// ensureModelAccess serves requires.services.llm for a tenant's apps.
//
// It fails the pass only for what the cluster itself refuses (reading or
// writing an object). A gateway that is not there, or does not answer, is
// not an error: the apps that declared it wait -- their Component reports
// why, and holds the release -- and every other app of the tenant is
// untouched by it.
func (r *TenantReconciler) ensureModelAccess(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	profiles map[string]*gentianov1alpha1.ComponentProfile,
) (modelAccessState, error) {
	var state modelAccessState
	logger := log.FromContext(ctx).WithName("model-access").WithValues("tenant", tenant.Name)

	installed := map[string]bool{}
	var declaring, undeclared []string
	for _, app := range tenant.Spec.Apps {
		name := app.Profile
		if name == "" && app.ProfileRef != nil {
			name = app.ProfileRef.Name
		}
		if name == "" {
			continue
		}
		installed[name] = true
		profile, ok := appProfileFromIndex(profiles, name)
		if !ok {
			// A profile that cannot be read says nothing about what the app
			// declared; nothing of the app's is touched.
			continue
		}
		if provisioner.MatchModelAccessProfile(profile) && !profile.IsAPI() {
			declaring = append(declaring, name)
		} else {
			undeclared = append(undeclared, name)
		}
	}

	// The components the platform placed on this tenant. One whose profile
	// declares the gateway, at platform trust, is served beside the apps;
	// any other is looked at for what it must not hold.
	placed, err := r.placedComponents(ctx, tenant)
	if err != nil {
		return state, err
	}
	direct := map[string]bool{}
	for _, name := range placed {
		if installed[name] {
			// An app of this name is the tenant's own install.
			continue
		}
		installed[name] = true
		profile, ok := appProfileFromIndex(profiles, name)
		if !ok {
			continue
		}
		if placedModelAccess(profile) && placedModelAccessRefusal(profile) == "" {
			declaring = append(declaring, name)
			direct[name] = true
		} else {
			undeclared = append(undeclared, name)
		}
	}

	recorded, err := r.provisionedStores(ctx, tenant.Name)
	if err != nil {
		return state, err
	}

	// What is there to take away: an installed app that does not declare the
	// gateway and still holds what an earlier version gave it, and an
	// uninstalled app whose key on record is still the one made from names.
	var stale []string
	for _, app := range undeclared {
		held, err := r.holdsModelCredentials(ctx, tenant, app)
		if err != nil {
			return state, err
		}
		if held || recorded[app].ModelKey != "" {
			stale = append(stale, app)
		}
	}
	var retained []string
	for app, p := range recorded {
		if installed[app] || p.ModelKey == "" {
			continue
		}
		// A key on record for a component the platform places, which this
		// tenant no longer has: the platform took the component away. It was
		// never the tenant's to uninstall, so nothing of it is retained.
		if profile, ok := appProfileFromIndex(profiles, app); ok && placedByPlatform(profile) {
			stale = append(stale, app)
			continue
		}
		retained = append(retained, app)
	}
	sort.Strings(stale)
	sort.Strings(retained)

	if len(declaring) == 0 && len(stale) == 0 && len(retained) == 0 {
		return state, nil
	}

	enabled := clusterLLMEnabled(ctx, r.Client)
	var gateway *modelgateway.Client
	gatewayWhy := "this cluster has no model gateway (the Cluster claim's llm.enabled is false)"
	if enabled {
		masterKey, err := r.getLiteLLMMasterKey(ctx)
		switch {
		case err == nil:
			gateway = litellmGateway(masterKey)
		case errors.IsNotFound(err):
			gatewayWhy = fmt.Sprintf("the model gateway's admin key (%s/%s) does not exist yet", litellmMasterKeyNS, litellmMasterKeySecret)
		default:
			gatewayWhy = fmt.Sprintf("the model gateway's admin key cannot be read: %v", err)
		}
	}

	// Take away first, so that an app that stopped declaring the gateway
	// loses its key in the pass that notices.
	for _, app := range stale {
		done, why, err := r.removeModelAccess(ctx, tenant, app, recorded[app], gateway, enabled)
		if err != nil {
			return state, err
		}
		if done {
			state.removed = append(state.removed, app)
		} else {
			state.waiting = append(state.waiting, app+": "+why)
		}
	}
	if gateway != nil {
		for _, app := range retained {
			removed, err := r.removeNamedModelKey(ctx, tenant, app, recorded[app].ModelKey, gateway)
			if err != nil {
				state.waiting = append(state.waiting, app+": "+err.Error())
				continue
			}
			if removed {
				state.removed = append(state.removed, app)
			}
		}
	}

	for _, app := range declaring {
		profile := profiles[app]
		if gateway == nil {
			state.waiting = append(state.waiting, app+": "+gatewayWhy)
			continue
		}
		if direct[app] {
			// On record before it is registered: nothing else writes a
			// placed component's key down, and the record is what a deleted
			// tenant's keys are found by.
			if err := r.recordModelAccess(ctx, tenant, app); err != nil {
				return state, err
			}
		}
		why, err := r.serveModelAccess(ctx, tenant, app, profile, gateway)
		if err != nil {
			return state, err
		}
		if why != "" {
			state.waiting = append(state.waiting, app+": "+why)
			continue
		}
		state.served = append(state.served, app)
	}

	if len(state.removed) > 0 {
		logger.Info("model access removed from apps that do not declare the model gateway", "apps", state.removed)
	}
	if len(state.waiting) > 0 {
		logger.Info("model access is waiting", "apps", state.waiting)
	}
	return state, nil
}

// serveModelAccess gives one declaring app its key: generated, registered,
// delivered, in that order. why is non-empty, with no error, when the gateway
// or the vault did not do its part and the app waits for the next pass.
//
// The Secret is written last. An app whose Secret exists holds a key the
// gateway accepts; the Component waits for the Secret before it installs the
// release, so no app starts with a key the gateway has not heard of.
func (r *TenantReconciler) serveModelAccess(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	app string,
	profile *gentianov1alpha1.ComponentProfile,
	gateway *modelgateway.Client,
) (why string, err error) {
	baseURL := modelgateway.OpenAIBaseURL(gateway.BaseURL)
	key, why, err := r.modelKey(ctx, tenant, app, baseURL)
	if err != nil || why != "" {
		return why, err
	}
	alias := modelgateway.KeyAlias(tenant.Name, app)
	replaced, err := gateway.EnsureKey(ctx, alias, key)
	if err != nil {
		return fmt.Sprintf("the model gateway did not register the app's key: %v", err), nil
	}
	if replaced {
		log.FromContext(ctx).Info("the app's key at the model gateway was replaced by the one the vault holds; "+
			"the key registered before no longer authenticates", "tenant", tenant.Name, "app", app, "alias", alias)
	}

	data := map[string]string{
		modelCredentialsBaseKey:    baseURL,
		modelCredentialsBaseURLKey: baseURL,
		modelCredentialsAPIKey:     key,
	}
	// Extra deterministic keys the profile asked for. They have always been
	// delivered in this Secret, and the profile that asks for one consumes
	// the Secret whole.
	for _, dsk := range profile.DerivedSecrets() {
		data[dsk.Key] = derivedSecretValue(tenant.Name, app)
	}
	return "", r.writeModelCredentials(ctx, tenant, app, data)
}

// modelKey is the key an app presents: what the vault holds for it, made
// there on first use. On an operator that runs without a vault the key is
// random and kept in the app's Secret, which is then its only copy.
func (r *TenantReconciler) modelKey(ctx context.Context, tenant *gentianov1alpha1.Tenant, app, baseURL string) (key, why string, err error) {
	if r.Seeder != nil {
		creds, err := r.Seeder.SeedModelAccess(ctx, tenant.Name, app, modelgateway.KeyPrefix, baseURL)
		if err != nil {
			return "", fmt.Sprintf("the vault did not keep the app's key: %v", err), nil
		}
		return creds.APIKey, "", nil
	}
	existing := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: modelCredentialsSecretName(app), Namespace: tenantNamespaceName(tenant)}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return "", "", err
	}
	if err == nil && ownsModelCredentials(existing, tenant.Name, app) {
		held := string(existing.Data[modelCredentialsAPIKey])
		if held != "" && held != modelgateway.LegacyKey(tenant.Name, app) {
			return held, "", nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate a model key: %w", err)
	}
	return modelgateway.KeyPrefix + hex.EncodeToString(buf), "", nil
}

// writeModelCredentials writes the app's Secret. A Secret of that name that
// is not this app's -- another's, or a person's -- is left as it is and
// reported: the name is the app's by convention only.
func (r *TenantReconciler) writeModelCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string, data map[string]string) error {
	key := types.NamespacedName{Name: modelCredentialsSecretName(app), Namespace: tenantNamespaceName(tenant)}
	labels := map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
		appLabel:       app,
	}
	raw := make(map[string][]byte, len(data))
	for k, v := range data {
		raw[k] = []byte(v)
	}
	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       raw,
		})
	}
	if err != nil {
		return err
	}
	if !ownsModelCredentials(existing, tenant.Name, app) {
		return fmt.Errorf("secret %s/%s exists and is not the operator's for app %s; it is not overwritten", key.Namespace, key.Name, app)
	}
	same := len(existing.Data) == len(data)
	for k, v := range data {
		same = same && string(existing.Data[k]) == v
	}
	if same {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = labels
	existing.Data = raw
	return r.Patch(ctx, existing, patch)
}

// ownsModelCredentials reports whether a Secret is the one the operator
// wrote for this app of this tenant. Nothing is deleted or overwritten on
// the strength of its name alone.
func ownsModelCredentials(secret *corev1.Secret, tenantName, app string) bool {
	return secret.Labels[managedByLabel] == managedByValue &&
		secret.Labels[tenantLabel] == tenantName &&
		secret.Labels[appLabel] == app
}

// holdsModelCredentials reports whether the operator's Secret for the app is
// still in the tenant's namespace.
func (r *TenantReconciler) holdsModelCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) (bool, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: modelCredentialsSecretName(app), Namespace: tenantNamespaceName(tenant)}, secret)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ownsModelCredentials(secret, tenant.Name, app), nil
}

// removeModelAccess takes away what an installed app that does not declare
// the gateway was given: its key at the gateway, by the alias on record, its
// Secret, and the entry in the record -- in that order, so that the record
// says the app holds a key for as long as the gateway might still accept one.
//
// Only the app's own: the alias is the one recorded for this app of this
// tenant, and the Secret is deleted only when it carries the operator's
// labels for it.
//
// done is false, with why, when the key is on record and the gateway that
// would remove it is not answering; the Secret is removed regardless, since
// nothing the app declared consumes it.
func (r *TenantReconciler) removeModelAccess(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	app string,
	recorded backup.Provisioned,
	gateway *modelgateway.Client,
	enabled bool,
) (done bool, why string, err error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: modelCredentialsSecretName(app), Namespace: tenantNamespaceName(tenant)}
	switch err := r.Get(ctx, key, secret); {
	case errors.IsNotFound(err):
	case err != nil:
		return false, "", err
	case ownsModelCredentials(secret, tenant.Name, app):
		if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return false, "", fmt.Errorf("remove the model credentials of %s: %w", app, err)
		}
	}
	if recorded.ModelKey == "" {
		return true, "", nil
	}
	switch {
	case gateway != nil:
		if _, err := gateway.DeleteKey(ctx, recorded.ModelKey); err != nil {
			return false, fmt.Sprintf("its key %s could not be removed from the model gateway: %v", recorded.ModelKey, err), nil
		}
	case enabled:
		// A gateway that should be there and cannot be asked: the key may
		// still authenticate, so it stays on record until it is removed.
		return false, fmt.Sprintf("its key %s is still on record and the model gateway cannot be asked to remove it", recorded.ModelKey), nil
	}
	// With no gateway on the cluster there is no key left: the gateway's
	// keys went with it (the same reading a purge takes).
	if err := r.forgetModelAccess(ctx, tenant.Name, app); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// removeNamedModelKey deals with the key of an app that is on record and no
// longer installed. Uninstalling keeps an app's model access, so a key the
// platform generated stays. One that is still the text made from the
// tenant's and the app's names does not: nothing runs that presents it, and
// whoever knows the two names can. It is deleted at the gateway and taken off
// the record; an app installed again that declares the gateway is given a
// generated key then.
func (r *TenantReconciler) removeNamedModelKey(ctx context.Context, tenant *gentianov1alpha1.Tenant, app, alias string, gateway *modelgateway.Client) (bool, error) {
	named, err := gateway.KeyIs(ctx, alias, modelgateway.LegacyKey(tenant.Name, app))
	if err != nil || !named {
		return false, err
	}
	if _, err := gateway.DeleteKey(ctx, alias); err != nil {
		return false, err
	}
	return true, r.forgetModelAccess(ctx, tenant.Name, app)
}

// forgetModelAccess takes an app's model key off the tenant's record of what
// was provisioned, once the key is gone.
func (r *TenantReconciler) forgetModelAccess(ctx context.Context, tenantName, app string) error {
	key := backup.ProvisionedRecordKey(tenantName)
	for attempt := 0; ; attempt++ {
		record := &corev1.ConfigMap{}
		reader := client.Reader(r.Client)
		if r.APIReader != nil {
			reader = r.APIReader
		}
		if err := reader.Get(ctx, key, record); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("read the record of what was provisioned for %s: %w", tenantName, err)
		}
		changed, err := backup.ForgetProvisioned(record, app, backup.KindModelAccess)
		if err != nil || !changed {
			return err
		}
		err = r.Update(ctx, record)
		if err == nil {
			return nil
		}
		// A purge and this pass's own recording write the same object.
		if !errors.IsConflict(err) || attempt >= 4 {
			return fmt.Errorf("update the record of what was provisioned for %s: %w", tenantName, err)
		}
	}
}

// placedByPlatform reports whether a profile is one the platform places on
// tenants itself, rather than one a tenant installs.
func placedByPlatform(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil &&
		(profile.Spec.DefaultForTenants || profile.Spec.DefaultForPlatform || profile.Spec.DefaultWhereStoreOffered)
}

// placedModelAccess reports whether a profile declares the model gateway for
// a component the platform places. Such a component is rendered by the
// Component reconciler, not by the app Composition (composedDelivery).
func placedModelAccess(profile *gentianov1alpha1.ComponentProfile) bool {
	return placedByPlatform(profile) && provisioner.MatchModelAccessProfile(profile) && !profile.IsAPI()
}

// placedModelAccessRefusal says why a placed component's declaration is not
// served, or "" when it is.
//
// Only a profile of platform trust: a placed component is given a key at the
// gateway on every tenant without any tenant having asked for it, which is
// the platform's to decide and not a catalogue entry's. And no key among the
// release values: the operator renders this release, and its values are read
// by whoever may read the release.
func placedModelAccessRefusal(profile *gentianov1alpha1.ComponentProfile) string {
	if profile.Spec.TrustTier != gentianov1alpha1.TrustTierPlatform {
		return fmt.Sprintf("%s declares requires.services.llm and is placed on tenants by the platform; "+
			"that is served only for a profile of trustTier platform, and this one is %q", profile.Name, profile.Spec.TrustTier)
	}
	if m := profile.Spec.Package.ValueMapping; m != nil && m.LLM != nil && m.LLM.APIKeyKey != "" {
		return fmt.Sprintf("%s maps the model key to a chart value (valueMapping.llm.apiKeyKey); a component the platform places "+
			"is told the Secret's name instead (valueMapping.llm.secretNameKey), so that the key is no release value", profile.Name)
	}
	return ""
}

// modelAccessOptional reports whether a profile's model gateway requirement
// is one its component runs without.
func modelAccessOptional(profile *gentianov1alpha1.ComponentProfile) bool {
	return provisioner.MatchModelAccessProfile(profile) && profile.Services().LLM.Optional
}

// placedComponents are the names of the Components the platform placed on a
// tenant that are not on their way out.
func (r *TenantReconciler) placedComponents(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	list := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, list, client.InNamespace(tenantNamespaceName(tenant)),
		client.MatchingLabels{componentOriginLabel: componentOriginDefault}); err != nil {
		return nil, fmt.Errorf("list the components the platform placed on %s: %w", tenant.Name, err)
	}
	var out []string
	for i := range list.Items {
		comp := &list.Items[i]
		// Named after its profile (ensureComponent); one that is not was
		// placed by nothing here.
		if comp.DeletionTimestamp != nil || comp.Spec.ProfileRef.Name != comp.Name {
			continue
		}
		out = append(out, comp.Name)
	}
	sort.Strings(out)
	return out, nil
}

// recordModelAccess writes a placed component's key alias on the tenant's
// record of what was provisioned. An app's is written with its stores
// (recordProvisionedStores); a placed component is no entry of spec.apps and
// has no other.
func (r *TenantReconciler) recordModelAccess(ctx context.Context, tenant *gentianov1alpha1.Tenant, name string) error {
	key := backup.ProvisionedRecordKey(tenant.Name)
	record := &corev1.ConfigMap{}
	err := r.Get(ctx, key, record)
	create := errors.IsNotFound(err)
	if create {
		record = backup.NewProvisionedRecord(tenant.Name)
	} else if err != nil {
		return fmt.Errorf("read the record of what was provisioned for %s: %w", tenant.Name, err)
	}
	original := record.DeepCopy()
	changed, err := backup.RecordProvisioned(record, name, backup.Provisioned{ModelKey: modelgateway.KeyAlias(tenant.Name, name)})
	if err != nil {
		return err
	}
	switch {
	case create:
		if err := r.Create(ctx, record); err != nil {
			return fmt.Errorf("write the record of what was provisioned for %s: %w", tenant.Name, err)
		}
	case changed:
		if err := r.Patch(ctx, record, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("update the record of what was provisioned for %s: %w", tenant.Name, err)
		}
	}
	return nil
}

// modelAccessHold is the hold of modelAccessFor alone.
func modelAccessHold(ctx context.Context, c client.Client, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (reason, message string, err error) {
	v, err := modelAccessFor(ctx, c, comp, profile, tenant)
	return v.reason, v.message, err
}

// modelAccessVerdict is what the Component reconciler is told about a
// component's model gateway requirement.
type modelAccessVerdict struct {
	// reason and message hold the release when reason is not empty.
	reason, message string
	// placed says the component is one the platform placed and its profile
	// declares the gateway: its values and its network path are written by
	// the Component reconciler.
	placed bool
	// delivered says the placed component's key is in its Secret.
	delivered bool
}

// modelAccessFor answers whether a component that declares the model gateway
// has to wait, and why. It is asked by the Component reconciler before it
// installs anything, the way it asks for a database: a requirement that is
// not met holds the release, and the Component says which one.
//
// An optional requirement of a placed component holds nothing: the verdict
// says whether the key is delivered, and the component is rendered with or
// without it.
func modelAccessFor(ctx context.Context, c client.Client, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (modelAccessVerdict, error) {
	var v modelAccessVerdict
	if !provisioner.MatchModelAccessProfile(profile) || profile.IsAPI() {
		return v, nil
	}
	isApp := false
	for _, app := range tenant.Spec.Apps {
		if app.Profile == comp.Name || (app.ProfileRef != nil && app.ProfileRef.Name == comp.Name) {
			isApp = true
		}
	}
	v.placed = !isApp && placedByPlatform(profile) && comp.Labels[componentOriginLabel] == componentOriginDefault
	if v.placed {
		if refusal := placedModelAccessRefusal(profile); refusal != "" {
			v.placed = false
			v.reason, v.message = "ModelAccessUnsupported", refusal+"; nothing gives it a key"
			return v, nil
		}
	}
	optional := v.placed && modelAccessOptional(profile)
	if !clusterLLMEnabled(ctx, c) {
		if optional {
			return v, nil
		}
		v.reason, v.message = "ModelGatewayUnavailable", fmt.Sprintf(
			"this cluster has no model gateway: %s declares requires.services.llm, and the Cluster claim's llm.enabled is false. "+
				"Nothing is installed until the cluster serves models", profile.Name)
		return v, nil
	}
	if !isApp && !v.placed {
		v.reason, v.message = "ModelAccessUnsupported", fmt.Sprintf(
			"%s declares requires.services.llm, which is served for the apps a tenant installs (Tenant.spec.apps) "+
				"and for the components the platform places on tenants; "+
				"%s is neither for tenant %s, so nothing gives it a key", profile.Name, comp.Name, tenant.Name)
		return v, nil
	}
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: modelCredentialsSecretName(comp.Name), Namespace: comp.Namespace}, secret)
	switch {
	case errors.IsNotFound(err):
		v.reason, v.message = "ModelAccessPending", fmt.Sprintf(
			"waiting for the model gateway to register a key for %s: the Secret %s does not exist yet",
			comp.Name, modelCredentialsSecretName(comp.Name))
	case err != nil:
		return v, err
	case strings.TrimSpace(string(secret.Data[modelCredentialsAPIKey])) == "":
		v.reason, v.message = "ModelAccessPending", fmt.Sprintf("the Secret %s carries no key yet", secret.Name)
	case v.placed && !ownsModelCredentials(secret, tenant.Name, comp.Name):
		// The name is the component's by convention only. A Secret somebody
		// else put there is not mounted into a component of platform trust.
		v.reason, v.message = "ModelAccessPending", fmt.Sprintf(
			"the Secret %s is not the one the operator writes for %s; it is not used", secret.Name, comp.Name)
	default:
		v.delivered = v.placed
	}
	if optional {
		v.reason, v.message = "", ""
	}
	return v, nil
}

// modelAccessValues tells a placed component's chart about the gateway, where
// its profile's valueMapping.llm says the chart takes it: the address, the
// name of the Secret its key is in, and whether there is a gateway for it at
// all. While an optional requirement is not met the address and the name are
// empty and available is false, so a chart renders the same keys either way.
func modelAccessValues(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, delivered bool) map[string]interface{} {
	out := map[string]interface{}{}
	if profile.Spec.Package.ValueMapping == nil || profile.Spec.Package.ValueMapping.LLM == nil {
		return out
	}
	m := profile.Spec.Package.ValueMapping.LLM
	baseURL, secretName := "", ""
	if delivered {
		baseURL = modelgateway.OpenAIBaseURL(litellmProxyBaseURL)
		secretName = modelCredentialsSecretName(comp.Name)
	}
	if m.BaseURLKey != "" {
		setPath(out, m.BaseURLKey, baseURL)
	}
	if m.SecretNameKey != "" {
		setPath(out, m.SecretNameKey, secretName)
	}
	if m.AvailableKey != "" {
		setPath(out, m.AvailableKey, delivered)
	}
	return out
}

// modelGatewayEgress is the way to the model gateway for a placed component
// whose key is delivered: the gateway's namespace on the gateway's port, and
// nothing else there. An app's is a rule of its kernel-access policy
// (internal/kernel/netpolicy); a placed component has none, so it is a rule
// of the component's own.
func modelGatewayEgress() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(provisioner.ModelGatewayPort)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": llmNamespace},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}

// componentsOfModelCredentials re-runs a Component when the Secret its model
// credentials are delivered in appears, changes or goes: a placed component
// whose requirement is optional was released without it and is rendered
// again with it.
func componentsOfModelCredentials() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		name, ok := strings.CutPrefix(obj.GetName(), modelCredentialsSecretName(""))
		if !ok || name == "" || obj.GetLabels()[managedByLabel] != managedByValue || obj.GetLabels()[appLabel] != name {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: obj.GetNamespace()}}}
	})
}
