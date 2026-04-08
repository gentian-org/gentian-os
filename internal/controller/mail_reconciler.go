/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	conditionMailReady  = "MailReady"
	mailRequeueAfter    = 30 * time.Second
	dkimKeySize         = 2048
	postfixChartRepo    = "https://bokysan.github.io/docker-postfix"
	postfixChartName    = "mail"
	postfixChartVersion = "4.0.0"
	dovecotChartRepo    = "https://docker-mailserver.github.io/docker-mailserver-helm"
	dovecotChartName    = "docker-mailserver"
	dovecotChartVersion = "v4.1.0"
)

// ensureMail provisions the mail stack for the tenant according to spec.mail.mode.
// It dispatches to one of four mode-specific handlers and sets the MailReady condition.
// Returns a non-zero RequeueAfter when waiting for Application CRs to become Healthy.
func (r *TenantReconciler) ensureMail(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	mode := gentianov1alpha1.MailModeSelfhosted
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Mode != "" {
		mode = tenant.Spec.Mail.Mode
	}

	switch mode {
	case gentianov1alpha1.MailModeSelfhosted:
		done, err := r.ensureMailSelfhosted(ctx, tenant)
		if err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
			return ctrl.Result{}, err
		}
		if !done {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
				"Provisioning", "Waiting for Postfix and Dovecot Application CRs to become Healthy")
			// No timer: the ArgoCD Application Watch re-triggers reconciliation when
			// Application health transitions. In the meantime the MailReady condition
			// stays False/Provisioning.
			return ctrl.Result{}, nil
		}
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"Selfhosted", "Postfix and Dovecot are deployed")
		return ctrl.Result{}, nil

	case gentianov1alpha1.MailModeExternal:
		// Guard first: the SmtpCredentialsSecret reference is mandatory for this mode.
		if tenant.Spec.Mail == nil || tenant.Spec.Mail.SmtpCredentialsSecret == "" {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
				"MissingConfig", "spec.mail.smtpCredentialsSecret is required for mode=external")
			return ctrl.Result{}, nil
		}
		done, err := r.ensureMailExternal(ctx, tenant)
		if err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
			return ctrl.Result{}, err
		}
		if !done {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
				"Provisioning", "Waiting for SMTP credentials to be available")
			return ctrl.Result{RequeueAfter: mailRequeueAfter}, nil
		}
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"External", "SMTP credentials have been propagated to the tenant namespace")
		return ctrl.Result{}, nil

	case gentianov1alpha1.MailModeTransportOnly:
		done, err := r.ensureMailTransportOnly(ctx, tenant)
		if err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
			return ctrl.Result{}, err
		}
		if !done {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
				"Provisioning", "Waiting for Postfix relay Application CR to become Healthy")
			// No timer: rely on ArgoCD Application Watch to re-trigger.
			return ctrl.Result{}, nil
		}
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"TransportOnly", "Postfix relay is deployed")
		return ctrl.Result{}, nil

	case gentianov1alpha1.MailModeDisabled:
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"MailDisabled", "Mail stack is disabled for this tenant")
		return ctrl.Result{}, nil

	default:
		r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
			"UnknownMode", fmt.Sprintf("unknown mail mode %q", mode))
		return ctrl.Result{}, nil
	}
}

// ensureMailSelfhosted provisions a full per-tenant mail stack:
//  1. A DKIM RSA-2048 private key Secret in the tenant namespace (created once; never
//     auto-rotated). The public key and suggested DNS records are written to TenantStatus.
//  2. An ArgoCD Application CR for Postfix (MTA) in the argocd namespace.
//  3. An ArgoCD Application CR for Dovecot (MDA/IMAP) in the argocd namespace.
//
// Returns true once both Application CRs report status.health.status == "Healthy".
func (r *TenantReconciler) ensureMailSelfhosted(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	nsName := tenantNamespaceName(tenant)

	// 1. DKIM key Secret (idempotent — generate once, never rotate automatically).
	pubKey, err := r.ensureDKIMSecret(ctx, tenant, nsName)
	if err != nil {
		return false, fmt.Errorf("ensure DKIM key Secret: %w", err)
	}
	if pubKey != "" {
		domain := mailDomain(tenant)
		if tenant.Status.Mail == nil {
			tenant.Status.Mail = &gentianov1alpha1.TenantMailStatus{}
		}
		tenant.Status.Mail.DKIMPublicKey = pubKey
		tenant.Status.Mail.SPFRecord = "v=spf1 mx ~all"
		tenant.Status.Mail.DMARCRecord = fmt.Sprintf("v=DMARC1; p=none; rua=mailto:dmarc@%s", domain)
	}

	// 2. Postfix Application CR.
	postfixDone, err := r.ensureMailApplication(ctx, buildPostfixApplication(tenant))
	if err != nil {
		return false, fmt.Errorf("ensure Postfix Application CR: %w", err)
	}

	// 3. Dovecot Application CR.
	dovecotDone, err := r.ensureMailApplication(ctx, buildDovecotApplication(tenant))
	if err != nil {
		return false, fmt.Errorf("ensure Dovecot Application CR: %w", err)
	}

	return postfixDone && dovecotDone, nil
}

// ensureMailExternal propagates SMTP relay credentials from the kernel namespace into
// the tenant namespace as a plain Kubernetes Secret. The source secret is identified by
// spec.mail.smtpCredentialsSecret (name in the kernel namespace).
//
// The caller (ensureMail) is responsible for validating that SmtpCredentialsSecret is
// non-empty before invoking this function.
//
// Returns false (with a requeue) while the source secret is not yet present.
func (r *TenantReconciler) ensureMailExternal(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	nsName := tenantNamespaceName(tenant)
	srcName := tenant.Spec.Mail.SmtpCredentialsSecret

	// Fetch the source Secret from the kernel namespace.
	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: srcName, Namespace: kernelNamespace}, src); err != nil {
		if errors.IsNotFound(err) {
			return false, nil // source not yet available — requeue
		}
		return false, fmt.Errorf("get SMTP credentials secret %s/%s: %w", kernelNamespace, srcName, err)
	}

	// Create the Secret in the tenant namespace if it does not already exist.
	dstName := smtpCredentialsSecretName(tenant.Name)
	dst := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: dstName, Namespace: nsName}, dst)
	if errors.IsNotFound(err) {
		desired := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dstName,
				Namespace: nsName,
				Labels: map[string]string{
					tenantLabel:    tenant.Name,
					managedByLabel: managedByValue,
				},
			},
			Data: src.Data,
			Type: src.Type,
		}
		if createErr := r.Create(ctx, desired); createErr != nil {
			return false, createErr
		}
		return true, nil // Secret was just created; credentials are now available.
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ensureMailTransportOnly provisions a shared Postfix relay for outbound delivery only.
// No Dovecot (no IMAP storage). Returns true when the Postfix Application CR is Healthy.
func (r *TenantReconciler) ensureMailTransportOnly(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	return r.ensureMailApplication(ctx, buildPostfixApplication(tenant))
}

// ensureMailApplication creates or checks health of an ArgoCD Application CR.
// Returns true when the Application reports status.health.status == "Healthy".
func (r *TenantReconciler) ensureMailApplication(ctx context.Context, desired *unstructured.Unstructured) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: argocdNamespace}, obj)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, desired)
	}
	if err != nil {
		return false, err
	}
	return argocdApplicationIsHealthy(obj), nil
}

// ensureDKIMSecret creates a DKIM RSA-2048 private key Secret in the tenant namespace
// if one does not already exist. Returns the base64-encoded PKIX DER public key suitable
// for publishing in a DKIM TXT record, or "" if the secret was just created (public key
// will be derived on the next reconcile pass).
func (r *TenantReconciler) ensureDKIMSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) (string, error) {
	secretName := dkimSecretName(tenant.Name)
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, existing)
	if err == nil {
		// Secret already exists — derive the public key from the stored private key.
		privPEM, ok := existing.Data["tls.key"]
		if !ok {
			return "", fmt.Errorf("DKIM secret %s/%s is missing key tls.key", nsName, secretName)
		}
		block, _ := pem.Decode(privPEM)
		if block == nil {
			return "", fmt.Errorf("DKIM secret %s/%s: tls.key is not valid PEM", nsName, secretName)
		}
		priv, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes)
		if parseErr != nil {
			return "", fmt.Errorf("parse DKIM private key in %s/%s: %w", nsName, secretName, parseErr)
		}
		pubDER, marshalErr := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if marshalErr != nil {
			return "", fmt.Errorf("marshal DKIM public key for %s/%s: %w", nsName, secretName, marshalErr)
		}
		return base64.StdEncoding.EncodeToString(pubDER), nil
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	// Generate a fresh RSA-2048 key pair.
	priv, err := rsa.GenerateKey(rand.Reader, dkimKeySize)
	if err != nil {
		return "", fmt.Errorf("generate DKIM RSA key for tenant %s: %w", tenant.Name, err)
	}
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal DKIM public key for tenant %s: %w", tenant.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Data: map[string][]byte{
			"tls.key": privPEM,
		},
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("create DKIM secret %s/%s: %w", nsName, secretName, err)
	}
	return base64.StdEncoding.EncodeToString(pubDER), nil
}

// deleteMail removes mail resources for a tenant during deletion.
//
// Application CRs (Postfix, Dovecot) are always removed so ArgoCD garbage-collects the
// deployed Helm releases. Under DeletionPolicy=Delete the DKIM key Secret and SMTP
// credentials Secret are also removed; under Retain they are preserved.
func (r *TenantReconciler) deleteMail(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	mode := gentianov1alpha1.MailModeSelfhosted
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Mode != "" {
		mode = tenant.Spec.Mail.Mode
	}

	// Remove ArgoCD Application CRs (always — these are ephemeral workload resources).
	appNames := mailApplicationNames(tenant.Name, mode)
	for _, name := range appNames {
		appCR := &unstructured.Unstructured{}
		appCR.SetGroupVersionKind(argocdApplicationGVK)
		appCR.SetName(name)
		appCR.SetNamespace(argocdNamespace)
		if err := r.Delete(ctx, appCR); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete mail Application CR %s: %w", name, err)
		}
	}

	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}

	// DeletionPolicy=Delete: also remove Secrets from the tenant namespace.
	nsName := tenantNamespaceName(tenant)
	if mode == gentianov1alpha1.MailModeSelfhosted {
		dkimSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: dkimSecretName(tenant.Name), Namespace: nsName,
		}}
		if err := r.Delete(ctx, dkimSec); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete DKIM secret for tenant %s: %w", tenant.Name, err)
		}
	}
	if mode == gentianov1alpha1.MailModeExternal {
		smtpSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: smtpCredentialsSecretName(tenant.Name), Namespace: nsName,
		}}
		if err := r.Delete(ctx, smtpSec); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete SMTP credentials secret for tenant %s: %w", tenant.Name, err)
		}
	}
	return nil
}

// --- ArgoCD Application CR constructors -------------------------------------

// buildPostfixApplication returns an ArgoCD Application CR that deploys a Postfix MTA
// into the tenant namespace. Used by both selfhosted and transport-only modes.
func buildPostfixApplication(tenant *gentianov1alpha1.Tenant) *unstructured.Unstructured {
	nsName := tenantNamespaceName(tenant)
	domain := mailDomain(tenant)
	// ALLOWED_SENDER_DOMAINS must be set or Postfix refuses to start.
	helmValues := fmt.Sprintf("config:\n  general:\n    ALLOWED_SENDER_DOMAINS: %q\n", domain)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	obj.SetName(postfixApplicationName(tenant.Name))
	obj.SetNamespace(argocdNamespace)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedField(obj.Object, "default", "spec", "project")
	_ = unstructured.SetNestedField(obj.Object, postfixChartRepo, "spec", "source", "repoURL")
	_ = unstructured.SetNestedField(obj.Object, postfixChartName, "spec", "source", "chart")
	_ = unstructured.SetNestedField(obj.Object, postfixChartVersion, "spec", "source", "targetRevision")
	_ = unstructured.SetNestedField(obj.Object, helmValues, "spec", "source", "helm", "values")
	_ = unstructured.SetNestedField(obj.Object, "https://kubernetes.default.svc", "spec", "destination", "server")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "destination", "namespace")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "prune")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "selfHeal")
	return obj
}

// buildDovecotApplication returns an ArgoCD Application CR that deploys a Dovecot MDA
// (IMAP/POP3) into the tenant namespace. Used by selfhosted mode only.
func buildDovecotApplication(tenant *gentianov1alpha1.Tenant) *unstructured.Unstructured {
	nsName := tenantNamespaceName(tenant)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	obj.SetName(dovecotApplicationName(tenant.Name))
	obj.SetNamespace(argocdNamespace)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedField(obj.Object, "default", "spec", "project")
	_ = unstructured.SetNestedField(obj.Object, dovecotChartRepo, "spec", "source", "repoURL")
	_ = unstructured.SetNestedField(obj.Object, dovecotChartName, "spec", "source", "chart")
	_ = unstructured.SetNestedField(obj.Object, dovecotChartVersion, "spec", "source", "targetRevision")
	_ = unstructured.SetNestedField(obj.Object, "https://kubernetes.default.svc", "spec", "destination", "server")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "destination", "namespace")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "prune")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "selfHeal")
	return obj
}

// --- Name helpers ------------------------------------------------------------

// mailDomain returns the effective mail domain for a tenant,
// defaulting to spec.domain when spec.mail.domain is not set.
func mailDomain(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Domain != "" {
		return tenant.Spec.Mail.Domain
	}
	return tenant.Spec.Domain
}

// mailApplicationNames returns the ArgoCD Application CR names that the mail reconciler
// creates for the given mode, so deleteMail can enumerate them without re-creating all
// the Application CR builders.
func mailApplicationNames(tenantName string, mode gentianov1alpha1.MailMode) []string {
	switch mode {
	case gentianov1alpha1.MailModeSelfhosted:
		return []string{postfixApplicationName(tenantName), dovecotApplicationName(tenantName)}
	case gentianov1alpha1.MailModeTransportOnly:
		return []string{postfixApplicationName(tenantName)}
	default:
		return nil
	}
}

func postfixApplicationName(tenantName string) string {
	return fmt.Sprintf("postfix-%s", tenantName)
}

func dovecotApplicationName(tenantName string) string {
	return fmt.Sprintf("dovecot-%s", tenantName)
}

func dkimSecretName(tenantName string) string {
	return fmt.Sprintf("dkim-%s", tenantName)
}

func smtpCredentialsSecretName(tenantName string) string {
	return fmt.Sprintf("smtp-credentials-%s", tenantName)
}
