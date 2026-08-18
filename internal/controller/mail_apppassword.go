/*
Copyright 2026 Gentian Organization.

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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"golang.org/x/crypto/argon2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Per-app mail passwords.
//
// Identities live in Keycloak and an OIDC login never yields a password, so a
// mail client that cannot speak XOAUTH2 has nothing to present. Each user
// therefore gets a credential per client application — the pattern Google and
// Fastmail use — rather than one credential that opens every mailbox.
//
// Two artefacts per tenant, deliberately on opposite sides of the boundary:
//
//	the hashes, in the kernel namespace, for Dovecot to verify against;
//	the plaintexts, in the TENANT namespace, for that tenant's mail client.
//
// A tenant therefore holds its own users' credentials and nobody else's, which
// is the property a shared master password could not offer.
const (
	mailAppPasswordApp       = "nextcloud-mail"
	mailAppPasswordSeedName  = "mail-apppw-seed"
	mailAppPasswordTenantSec = "mail-app-passwords"
)

// Derived, not random. A random password per user would have to be stored to
// survive a reconcile, and storing it is the thing to avoid; deriving it from a
// per-tenant seed means the same address always yields the same password
// without either side keeping a list.
//
// The seed is per tenant, so the derivation cannot be replayed across tenants
// even by something holding one tenant's seed.
func deriveMailPassword(seed []byte, address string) string {
	mac := hmac.New(sha256.New, seed)
	mac.Write([]byte(mailAppPasswordApp + ":" + address))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:32]
}

// argon2idPasswdLine renders a Dovecot passwd-file entry.
//
// A hash, never the password: the file reaches Dovecot as a Kubernetes Secret,
// and Secrets are base64 in etcd rather than encrypted unless the API server
// runs with --encryption-provider-config. lint-password-schemes fails the build
// on any scheme that stores the credential itself.
func argon2idPasswdLine(address, password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const t, m, p, keyLen = 3, 64 * 1024, 4, 32
	sum := argon2.IDKey([]byte(password), salt, t, m, p, keyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("%s:{ARGON2ID}$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		address, m, t, p, b64(salt), b64(sum)), nil
}

// keycloakRealmUsers lists usernames in a realm.
//
// Read from Keycloak rather than from the mail system, because Keycloak is
// where a user comes into existence — a mailbox derived from anywhere else is a
// second registry that drifts the first time someone is added or removed.
func (r *TenantReconciler) keycloakRealmUsers(ctx context.Context, realm string) ([]string, error) {
	ns := defaultServicesNamespace()
	base, err := r.secretValue(ctx, keycloakAdminSecret, ns, "url")
	if err != nil {
		return nil, err
	}
	user, err := r.secretValue(ctx, keycloakAdminSecret, ns, "username")
	if err != nil {
		return nil, err
	}
	pass, err := r.secretValue(ctx, keycloakAdminSecret, ns, "password")
	if err != nil {
		return nil, err
	}
	base = strings.TrimSuffix(base, "/")

	form := url.Values{
		"client_id":  {"admin-cli"},
		"username":   {user},
		"password":   {pass},
		"grant_type": {"password"},
	}
	resp, err := http.PostForm(base+"/realms/master/protocol/openid-connect/token", form)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("keycloak returned no admin token (status %d)", resp.StatusCode)
	}

	// Paged: a realm with more than the default page of users would otherwise
	// have the tail silently omitted, and a missing mailbox reads as a mail
	// fault rather than a truncated list.
	var names []string
	for first := 0; ; first += 100 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/admin/realms/%s/users?first=%d&max=100", base, realm, first), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		page, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		var users []struct {
			Username string `json:"username"`
			Enabled  bool   `json:"enabled"`
		}
		err = json.NewDecoder(page.Body).Decode(&users)
		_ = page.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			if u.Enabled && u.Username != "" {
				names = append(names, u.Username)
			}
		}
		if len(users) < 100 {
			break
		}
	}
	sort.Strings(names)
	return names, nil
}

// tenantMailSeed returns the tenant's derivation seed, creating it once.
//
// Never rotated automatically: rotating it changes every password the tenant's
// clients already hold, which logs everyone out of their mail at once with no
// signal as to why.
func (r *TenantReconciler) tenantMailSeed(ctx context.Context, tenant string) ([]byte, error) {
	ns := defaultServicesNamespace()
	name := mailAppPasswordSeedName + "-" + tenant
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sec)
	if err == nil {
		if v, ok := sec.Data["seed"]; ok && len(v) > 0 {
			return v, nil
		}
	} else if !errors.IsNotFound(err) {
		return nil, err
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	create := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string][]byte{"seed": seed},
	}
	if errors.IsNotFound(err) {
		return seed, r.Create(ctx, create)
	}
	sec.Data = create.Data
	return seed, r.Update(ctx, sec)
}

// syncMailAppPasswords writes both halves for one tenant.
func (r *TenantReconciler) syncMailAppPasswords(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	name := tenant.Name
	domain := mailDomain(tenant, r.KernelDomain, r.TenancyMode)
	if domain == "" {
		return nil
	}
	seed, err := r.tenantMailSeed(ctx, name)
	if err != nil {
		return err
	}
	users, err := r.keycloakRealmUsers(ctx, name)
	if err != nil {
		return err
	}

	var lines strings.Builder
	plain := map[string][]byte{}
	for _, u := range users {
		addr := u + "@" + domain
		pw := deriveMailPassword(seed, addr)
		line, err := argon2idPasswdLine(addr, pw)
		if err != nil {
			return err
		}
		lines.WriteString(line + "\n")
		// Secret keys allow only [-._a-zA-Z0-9], and a Keycloak username is
		// frequently an email address — the @ makes the whole Secret invalid and
		// rejected, so every user's password is lost, not just that one's.
		plain[secretKeySafe(u)] = []byte(pw)
	}

	// The tenant's own copy, in its own namespace, for its mail client.
	if err := r.upsertSecret(ctx, mailAppPasswordTenantSec, tenantNamespaceName(tenant), plain); err != nil {
		return err
	}
	// The hashes, in the kernel namespace, for Dovecot.
	return r.upsertSecret(ctx, "dovecot-app-passwords", defaultServicesNamespace(), map[string][]byte{
		mailAppPasswordApp + ".users": []byte(lines.String()),
		mailAppPasswordApp + ".conf": []byte(fmt.Sprintf(
			"passdb {\n  driver = passwd-file\n  args = /etc/dovecot/apppw/%s.users\n"+
				"  result_failure = continue\n  result_internalfail = continue\n}\n",
			mailAppPasswordApp)),
	})
}

// secretKeySafe maps a username onto the charset a Secret key permits.
func secretKeySafe(s string) string {
	out := []rune(s)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_':
		default:
			out[i] = '_'
		}
	}
	return string(out)
}

func (r *TenantReconciler) upsertSecret(ctx context.Context, name, ns string, data map[string][]byte) error {
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sec)
	if errors.IsNotFound(err) {
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Labels: map[string]string{managedByLabel: managedByValue},
			},
			Data: data,
		})
	}
	if err != nil {
		return err
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range data {
		sec.Data[k] = v
	}
	return r.Update(ctx, sec)
}
