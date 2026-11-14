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
	stderrors "errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// IntegrationBindingReconciler reconciles a IntegrationBinding object
type IntegrationBindingReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes (internal/schemacheck). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
	client.Client
	Scheme *runtime.Scheme
	Seeder *secrets.Seeder
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=appgrants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=componentprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=helm.crossplane.io,resources=releases,verbs=get;list;watch;patch

// Reconcile reads that state of the cluster for a IntegrationBinding object and makes changes based on the state read
// and what is in the IntegrationBinding.Spec
func (r *IntegrationBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ib gentianov1alpha1.IntegrationBinding
	if err := r.Get(ctx, req.NamespacedName, &ib); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// What was granted is said whether or not the wiring below succeeds: a
	// binding nobody granted opens nothing, and a reader has to be able to
	// see that this, and no fault, is why the two apps cannot reach each
	// other.
	granted, err := r.grantedCondition(ctx, &ib)
	if err != nil {
		return ctrl.Result{}, err
	}
	apimeta.SetStatusCondition(&ib.Status.Conditions, granted)

	// The address is the consumer's once it was granted the contract, and
	// not before: until then nothing is opened to it. Where nothing granted
	// answers the contract any more, the tenant's reconcile empties it
	// (clearUngrantedContractEndpoints).
	var seedErr error
	var again time.Duration
	if granted.Reason != reasonNotGranted {
		var endpoint string
		endpoint, seedErr = r.seedContract(ctx, &ib)
		if seedErr == nil && endpoint != "" {
			again, seedErr = r.deliverAddress(ctx, &ib, endpoint)
		}
	}
	if seedErr == nil {
		ib.Status.State = gentianov1alpha1.IntegrationBindingStateReady
	}
	if err := r.Status().Update(ctx, &ib); err != nil {
		return ctrl.Result{}, err
	}
	if seedErr != nil {
		return ctrl.Result{}, seedErr
	}
	return ctrl.Result{RequeueAfter: again}, nil
}

// addressReadAnnotation holds, on a consumer's ExternalSecret, when the
// binding's reconciler last had it read again for a granted contract's
// address, in Unix seconds. Not force-sync: the credential catalog reads a
// fresh force-sync stamp as a value just stored and still pending.
const addressReadAnnotation = "gentianos.io/contract-address-read"

// addressDeliveredPrefix is the prefix of the annotation, on a consumer's
// Helm release, that names the address of one contract the release was last
// told to take: contract-address.gentianos.io/<contract>.
const addressDeliveredPrefix = "contract-address.gentianos.io/"

// addressReadEvery is how long the secrets operator is given to answer one
// such read before it is asked again, and addressReadWait how long until the
// reconciler looks whether it did.
const (
	addressReadEvery = time.Minute
	addressReadWait  = 10 * time.Second
)

// deliverAddress sees a granted contract's address through to the consumer.
//
// The consumer reads the address from the tenant's store through its values
// Secret, which the secrets operator refreshes once an hour, and its Helm
// release reads that Secret as a value, which the provider looks at on its
// own poll. Left to those two, a grant has no effect for up to an hour. So
// while the Secret holds another address the ExternalSecret is read again,
// and once it holds this one the release is changed, which is what makes the
// provider look at it now. Any change to either object's annotations does
// that; nothing of what they declare is touched.
//
// A consumer that maps no address for the contract has no such key and is
// left alone. The duration is when to look again: zero once delivered.
func (r *IntegrationBindingReconciler) deliverAddress(ctx context.Context, ib *gentianov1alpha1.IntegrationBinding, endpoint string) (time.Duration, error) {
	ns := ib.Spec.Consumer.Namespace
	if ns == "" {
		ns = ib.Namespace
	}
	name := ib.Spec.Consumer.App + "-sensitive-values"
	key := "integration-" + ib.Spec.Contract + "-endpoint"

	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, es); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get ExternalSecret %s/%s: %w", ns, name, err)
	}
	if !externalSecretReadsKey(es, key) {
		return 0, nil
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return 0, fmt.Errorf("get Secret %s/%s: %w", ns, name, err)
	}
	if err != nil || string(secret.Data[key]) != endpoint {
		now := time.Now()
		if r.Now != nil {
			now = r.Now()
		}
		if stamp, perr := strconv.ParseInt(es.GetAnnotations()[addressReadAnnotation], 10, 64); perr == nil &&
			now.Sub(time.Unix(stamp, 0)) < addressReadEvery {
			return addressReadWait, nil
		}
		if err := annotate(ctx, r.Client, es, addressReadAnnotation, strconv.FormatInt(now.Unix(), 10)); err != nil {
			return 0, fmt.Errorf("have ExternalSecret %s/%s read again: %w", ns, name, err)
		}
		return addressReadWait, nil
	}

	// The release a component is installed as: named after its namespace
	// and the component (releaseName).
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: ns + "-" + ib.Spec.Consumer.App}, release); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get release of %s: %w", ib.Spec.Consumer.App, err)
	}
	mark := addressDeliveredPrefix + ib.Spec.Contract
	if release.GetAnnotations()[mark] == endpoint {
		return 0, nil
	}
	if err := annotate(ctx, r.Client, release, mark, endpoint); err != nil {
		return 0, fmt.Errorf("have release %s read its values again: %w", release.GetName(), err)
	}
	return 0, nil
}

// externalSecretReadsKey reports whether an ExternalSecret fills the key of
// its Secret.
func externalSecretReadsKey(es *unstructured.Unstructured, key string) bool {
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	for _, raw := range data {
		if item, ok := raw.(map[string]any); ok && item["secretKey"] == key {
			return true
		}
	}
	return false
}

// annotate sets one annotation with a merge patch: the rest of the object is
// its owner's to write.
func annotate(ctx context.Context, c client.Client, obj *unstructured.Unstructured, key, value string) error {
	before := obj.DeepCopy()
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	obj.SetAnnotations(annotations)
	return c.Patch(ctx, obj, client.MergeFrom(before))
}

// conditionBindingGranted says how much of what a binding asks for the
// tenant's administrator granted.
const conditionBindingGranted = "Granted"

// reasonNotGranted is the Granted condition's reason where nothing of the
// contract was granted.
const reasonNotGranted = "NotGranted"

// grantedCondition compares what the binding asks for with the consumer's
// grant. True only when every capability asked for was granted.
func (r *IntegrationBindingReconciler) grantedCondition(ctx context.Context, ib *gentianov1alpha1.IntegrationBinding) (metav1.Condition, error) {
	var grants gentianov1alpha1.AppGrantList
	if err := r.List(ctx, &grants, client.InNamespace(ib.Namespace)); err != nil {
		return metav1.Condition{}, fmt.Errorf("list AppGrants in %s: %w", ib.Namespace, err)
	}
	var grant *gentianov1alpha1.AppGrant
	for i := range grants.Items {
		if grants.Items[i].Spec.App == ib.Spec.Consumer.App {
			grant = &grants.Items[i]
		}
	}
	effective := netpolicy.EffectiveContractCapabilities(ib, grant)
	have := make(map[string]struct{}, len(effective))
	for _, c := range effective {
		have[c] = struct{}{}
	}
	var missing []string
	for _, c := range ib.Spec.Capabilities {
		if _, ok := have[c]; !ok {
			missing = append(missing, c)
		}
	}
	cond := metav1.Condition{Type: conditionBindingGranted, ObservedGeneration: ib.Generation}
	switch {
	case len(effective) == 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonNotGranted
		cond.Message = fmt.Sprintf("%s asks for contract %s of %s and was granted none of it: nothing is opened between the two until the tenant's administrator grants it",
			ib.Spec.Consumer.App, ib.Spec.Contract, ib.Spec.Provider.App)
	case len(missing) > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PartlyGranted"
		cond.Message = fmt.Sprintf("granted: %s; asked for and not granted: %s",
			strings.Join(effective, ", "), strings.Join(missing, ", "))
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Granted"
		cond.Message = "granted: " + strings.Join(effective, ", ")
	}
	return cond, nil
}

// seedContract writes what the consumer needs to address the provider, and
// returns the address it is told: empty where the contract has none.
func (r *IntegrationBindingReconciler) seedContract(ctx context.Context, ib *gentianov1alpha1.IntegrationBinding) (string, error) {
	if r.Seeder == nil {
		return "", nil
	}
	endpoint := ""
	user := secrets.ContractUser(ib.Spec.Contract)
	switch {
	case ib.Spec.Endpoint != nil:
		// Where the provider's profile says it answers the contract. What
		// is asked there is the two apps' business.
		endpoint = contractURL(ib.Spec.Endpoint, ib.Namespace)
	case ib.Spec.Contract == "calendar", ib.Spec.Contract == "contacts":
		// The provider's profile, for the Service its contract is served on.
		var providerProfile gentianov1alpha1.ComponentProfile
		if err := r.Get(ctx, client.ObjectKey{Name: ib.Spec.Provider.App}, &providerProfile); err != nil {
			return "", fmt.Errorf("failed to get provider ComponentProfile: %w", err)
		}
		// The provider's own web entry, which is what its DAV endpoint is
		// served on. Its backend names the Service and the port; where
		// the entry routes to ANOTHER component, that component's Service
		// is the one to address, because that is where the contract is
		// actually answered.
		//
		// Addressed in-cluster rather than through the gateway: the
		// consumer is in the same namespace, the endpoint is for a
		// machine, and a round trip out to the edge and back would need a
		// session this has no business holding.
		for _, e := range providerProfile.GatewayExposures() {
			if e.Name != primaryExposureName || e.Backend.Service == "" {
				continue
			}
			endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/remote.php/dav/",
				e.Backend.Service, ib.Namespace, e.Backend.Port)
			break
		}
	}

	if endpoint != "" {
		// The path the binding names, which is the one the tenant's
		// reconciler seeded the password at and the app's release reads:
		// it is the tenant's, not the namespace's, and the two names differ.
		path := ""
		if ib.Spec.Auth != nil {
			path = ib.Spec.Auth.VaultPath
		}
		if path == "" {
			tenant := ib.Labels[tenantLabel]
			if tenant == "" {
				return "", fmt.Errorf("binding %s/%s names neither a vault path nor its tenant", ib.Namespace, ib.Name)
			}
			path = secrets.ContractPath(tenant, ib.Spec.Contract)
		}
		// A read that failed is not an empty path: writing the endpoint
		// over it would drop the shared password both apps hold.
		data, err := r.Seeder.Read(ctx, path)
		if err != nil && !stderrors.Is(err, secrets.ErrNotFound) {
			return "", fmt.Errorf("read contract data from Vault: %w", err)
		}
		if data == nil {
			data = make(map[string]string)
		}
		needsUpdate := false
		if data["endpoint"] != endpoint {
			data["endpoint"] = endpoint
			needsUpdate = true
		}
		if data["user"] != user {
			data["user"] = user
			needsUpdate = true
		}
		if needsUpdate {
			if err := r.Seeder.Write(ctx, path, data); err != nil {
				return "", fmt.Errorf("failed to write contract data to Vault: %w", err)
			}
		}
	}
	return endpoint, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IntegrationBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A grant that is made, changed or withdrawn changes what every binding
	// of its namespace has to say about itself.
	bindingsOfGrant := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var bindings gentianov1alpha1.IntegrationBindingList
		if err := r.List(ctx, &bindings, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(bindings.Items))
		for i := range bindings.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&bindings.Items[i])})
		}
		return out
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.IntegrationBinding{}).
		Watches(&gentianov1alpha1.AppGrant{}, handler.EnqueueRequestsFromMapFunc(bindingsOfGrant)).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.IntegrationBinding{}, r))
}
