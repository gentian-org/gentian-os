// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
)

const (
	authzRuntimeSecretName = "openfga-runtime"
	authzRequeueInterval   = 5 * time.Minute
)

// AuthzBridgeReconciler provisions OpenFGA and syncs Keycloak users into ReBAC tuples.
// Stage 1 greenfield path — no Nubus/LDAP dependency.
//
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
type AuthzBridgeReconciler struct {
	client.Client
	KernelRealm string
	OpenFGAURL  string
	OpenFGAToken string
	Enabled     bool
}

func (r *AuthzBridgeReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	if !r.Enabled {
		return reconcile.Result{}, nil
	}

	kcURL, kcUser, kcPass, err := r.loadKeycloakAdmin(ctx)
	if err != nil {
		logger.Error(err, "load keycloak-admin secret")
		return reconcile.Result{RequeueAfter: authzRequeueInterval}, nil
	}

	storeID, modelID := r.loadRuntimeIDs(ctx)
	bridge := &authz.Bridge{
		OpenFGA:  authz.NewOpenFGAClient(r.OpenFGAURL, r.OpenFGAToken),
		Keycloak: authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass),
		StoreID:  storeID,
		ModelID:  modelID,
	}
	if err := bridge.EnsureBootstrap(ctx); err != nil {
		logger.Error(err, "openfga bootstrap")
		return reconcile.Result{RequeueAfter: authzRequeueInterval}, nil
	}
	if err := r.persistRuntime(ctx, bridge.StoreID, bridge.ModelID); err != nil {
		logger.Error(err, "persist openfga runtime secret")
		return reconcile.Result{RequeueAfter: authzRequeueInterval}, nil
	}

	realms := []string{r.kernelRealm()}
	if req.Name != "" {
		tenant := &gentianov1alpha1.Tenant{}
		if err := r.Get(ctx, req.NamespacedName, tenant); err == nil && tenant.DeletionTimestamp == nil {
			realms = appendUniqueStrings(realms, keycloakRealmName(tenant))
		}
	}
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err == nil {
		for i := range tenantList.Items {
			if tenantList.Items[i].DeletionTimestamp != nil {
				continue
			}
			realms = appendUniqueStrings(realms, keycloakRealmName(&tenantList.Items[i]))
		}
	}

	for _, realm := range realms {
		if err := bridge.SyncRealmUsers(ctx, realm); err != nil {
			logger.Error(err, "sync realm users to openfga", "realm", realm)
			return reconcile.Result{RequeueAfter: authzRequeueInterval}, nil
		}
	}

	logger.Info("authz bridge sync complete", "store_id", bridge.StoreID, "realms", len(realms))
	return reconcile.Result{RequeueAfter: authzRequeueInterval}, nil
}

func (r *AuthzBridgeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if !r.Enabled {
		return nil
	}
	// Primary trigger: keycloak-admin Secret (Stage 1 runs before any Tenant CR exists).
	// Secondary: Tenant CR changes add per-tenant realms to the sync loop.
	return ctrl.NewControllerManagedBy(mgr).
		Named("authz-bridge").
		For(&corev1.Secret{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetNamespace() == kernelNamespace && obj.GetName() == keycloakAdminSecret
		}))).
		Watches(
			&gentianov1alpha1.Tenant{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(obj)}}
			}),
		).
		Complete(r)
}

func (r *AuthzBridgeReconciler) kernelRealm() string {
	if r.KernelRealm != "" {
		return r.KernelRealm
	}
	return "kernel"
}

func (r *AuthzBridgeReconciler) loadKeycloakAdmin(ctx context.Context) (url, user, pass string, err error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: keycloakAdminSecret, Namespace: kernelNamespace}, secret); err != nil {
		return "", "", "", err
	}
	url = string(secret.Data["url"])
	user = string(secret.Data["username"])
	if user == "" {
		user = "admin"
	}
	pass = string(secret.Data["password"])
	if url == "" || pass == "" {
		return "", "", "", fmt.Errorf("keycloak-admin secret missing url or password")
	}
	return url, user, pass, nil
}

func (r *AuthzBridgeReconciler) loadRuntimeIDs(ctx context.Context) (storeID, modelID string) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: authzRuntimeSecretName, Namespace: kernelNamespace}, secret)
	if err != nil {
		return "", ""
	}
	return string(secret.Data["store_id"]), string(secret.Data["model_id"])
}

func (r *AuthzBridgeReconciler) persistRuntime(ctx context.Context, storeID, modelID string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      authzRuntimeSecretName,
			Namespace: kernelNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				appLabel:       "openfga",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"store_id": storeID,
			"model_id": modelID,
			"api_url":  r.OpenFGAURL,
		},
	}
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: authzRuntimeSecretName, Namespace: kernelNamespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, secret)
	}
	if err != nil {
		return err
	}
	existing.Data = map[string][]byte{
		"store_id": []byte(storeID),
		"model_id": []byte(modelID),
		"api_url":  []byte(r.OpenFGAURL),
	}
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels[managedByLabel] = managedByValue
	existing.Labels[appLabel] = "openfga"
	return r.Update(ctx, existing)
}

// AuthzBridgeEnabled reports whether Stage 1 authz bridge should run.
func AuthzBridgeEnabled() bool {
	return os.Getenv("AUTHZ_BRIDGE_ENABLED") == "true"
}
