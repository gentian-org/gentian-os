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
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

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
	// The identity a tenant's Keycloak realm authenticates as when it submits
	// mail. It is an app like any other, so it reuses the machinery above rather
	// than introducing a second kind of credential — the realm is simply a mail
	// client that happens not to be operated by a person.
	mailSubmissionApp = "keycloak-smtp"
	// Local part of that identity. It never receives: Dovecot's static userdb
	// accepts any address in a registered domain, so a mailbox appears only if
	// something delivers to it, and nothing does.
	mailSubmissionLocalPart = "noreply"
	// Where the plaintext waits for the SMTP configuration Job, which runs in the
	// kernel namespace beside Keycloak rather than in the tenant's namespace.
	mailSubmissionSecretPrefix = "mail-submission-"
	// The tenant's apps share one submission identity, smtp-<tenant>, because
	// they share one Secret: that is what the app reconciler seeds into OpenBao
	// and injects as Helm values. Naming it as an app here only registers what
	// already exists.
	mailAppSubmissionApp = "tenant-apps"
	// The kernel realm's own identity, written by the installer rather than by
	// the operator, and registered from whatever that Secret holds.
	mailKernelRealmSubmissionApp = "kernel-realm"
)

// Derived, not random. A random password per user would have to be stored to
// survive a reconcile, and storing it is the thing to avoid; deriving it from a
// per-tenant seed means the same address always yields the same password
// without either side keeping a list.
//
// The seed is per tenant, so the derivation cannot be replayed across tenants
// even by something holding one tenant's seed.
// The app is part of the derivation, not decoration: without it a user's IMAP
// password and their realm's submission password would be the same string, so
// one credential would open both and "a credential per (user, app)" would be a
// claim the code does not keep.
func deriveMailPassword(seed []byte, app, address string) string {
	mac := hmac.New(sha256.New, seed)
	mac.Write([]byte(app + ":" + address))
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
// keycloakAdminHTTP is used for both calls below.
//
// http.DefaultClient has no timeout. A Keycloak that accepts the connection and
// then never answers — a wedged pod, a NetworkPolicy that blackholes rather than
// rejects — would block a controller-runtime worker forever, and the tenant
// whose reconcile owned that worker would simply stop being reconciled with
// nothing logged.
var keycloakAdminHTTP = &http.Client{Timeout: 30 * time.Second}

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
	// NewRequestWithContext, not http.PostForm: PostForm takes no context, so
	// this call could not be cancelled when the reconcile was.
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := keycloakAdminHTTP.Do(tokenReq)
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
		page, err := keycloakAdminHTTP.Do(req)
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

	// The lines already stored, so an unchanged user keeps its line instead of
	// being re-hashed with a new salt. Without this every reconcile rewrote the
	// whole file, and every rewrite scheduled the reconcile that rewrote it next.
	existing := map[string]string{}
	{
		sec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: "dovecot-app-passwords", Namespace: defaultServicesNamespace(),
		}, sec); err == nil {
			for _, l := range strings.Split(string(sec.Data[mailAppPasswordFile(mailAppPasswordApp, name)+".users"]), "\n") {
				if addr, _, ok := strings.Cut(strings.TrimSpace(l), ":"); ok {
					existing[addr] = strings.TrimSpace(l)
				}
			}
		} else if !errors.IsNotFound(err) {
			return err
		}
	}

	var lines strings.Builder
	plain := map[string][]byte{}
	for _, u := range users {
		// A Keycloak username is frequently already an email address, and
		// appending the domain to one yields christian@corp.gtn.host@corp.gtn.host
		// — an address Dovecot will never be asked about, so the user's mail
		// client authenticates against an entry that does not match and the
		// failure reads as a wrong password.
		addr := u
		if !strings.Contains(addr, "@") {
			addr = u + "@" + domain
		}
		pw := deriveMailPassword(seed, mailAppPasswordApp, addr)
		line := existing[addr]
		if !argon2idLineMatches(line, addr, pw) {
			var err error
			if line, err = argon2idPasswdLine(addr, pw); err != nil {
				return err
			}
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
	// The hashes, in the kernel namespace, for Dovecot — one pair of files per
	// tenant, NOT one pair shared by all of them.
	//
	// The shared key this replaces held only the lines of whichever tenant
	// reconciled last, because each reconcile rebuilds the value from one
	// realm's users and writes it whole. Two tenants therefore took turns: corp
	// reconciled and finnor's users could no longer authenticate, finnor
	// reconciled and corp's could not, each failing as a wrong password with
	// nothing logged beyond an auth failure. Dovecot includes the directory by
	// glob and the passdbs chain with result_failure = continue, so a file per
	// tenant needs no configuration change and cannot overwrite another's.
	if err := r.upsertSecret(ctx, "dovecot-app-passwords", defaultServicesNamespace(), map[string][]byte{
		mailAppPasswordFile(mailAppPasswordApp, name) + ".users": []byte(lines.String()),
		mailAppPasswordFile(mailAppPasswordApp, name) + ".conf":  passdbInclude(mailAppPasswordFile(mailAppPasswordApp, name)),
	}); err != nil {
		return err
	}
	// The pre-split shared file — but ONLY once every tenant has its own.
	//
	// Removing it as soon as the first tenant reconciled is what broke this in
	// production: the shared file held the users of whichever tenant wrote it
	// last, so deleting it took away the credentials of every tenant that had not
	// yet reconciled under the new scheme. Their logins failed as wrong passwords
	// until their own reconcile happened to come round, which for a Tenant that
	// reconciles on a timer can be a long time to be unable to read mail.
	//
	// Checking first costs one List and makes the migration order-independent:
	// whichever tenant reconciles last is the one that clears it away.
	migrated, err := r.allTenantsHaveOwnPasswdFile(ctx)
	if err != nil {
		return err
	}
	if migrated {
		if err := r.deleteSecretKeys(ctx, "dovecot-app-passwords", defaultServicesNamespace(),
			mailAppPasswordApp+".users", mailAppPasswordApp+".conf"); err != nil {
			return err
		}
	}
	return r.syncMailSubmissionCredential(ctx, tenant, domain, seed)
}

// allTenantsHaveOwnPasswdFile reports whether every Tenant now has its own
// per-tenant passwd-file, which is the condition for retiring the shared one.
func (r *TenantReconciler) allTenantsHaveOwnPasswdFile(ctx context.Context) (bool, error) {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: "dovecot-app-passwords", Namespace: defaultServicesNamespace(),
	}, sec); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return false, err
	}
	for i := range tenants.Items {
		if _, ok := sec.Data[mailAppPasswordFile(mailAppPasswordApp, tenants.Items[i].Name)+".users"]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// mailPasswdFileKeys lists every passdb key one tenant owns, across apps.
//
// One place to enumerate them, so that deleting a tenant cannot leave a
// credential behind because a later app was added to the writer and not to the
// remover.
func mailPasswdFileKeys(tenant string) []string {
	var keys []string
	for _, app := range []string{mailAppPasswordApp, mailSubmissionApp, mailAppSubmissionApp} {
		keys = append(keys,
			mailAppPasswordFile(app, tenant)+".users",
			mailAppPasswordFile(app, tenant)+".conf")
	}
	return keys
}

// mailAppPasswordFile names one tenant's passwd-file for one app.
func mailAppPasswordFile(app, tenant string) string {
	return app + "-" + tenant
}

// passdbInclude renders the Dovecot passdb block that reads one such file.
//
// result_failure = continue so a user present in one app's file and absent from
// another's is not rejected by the first file that lacks them; the empty
// passwd-file Dovecot includes last is still the final deny.
func passdbInclude(file string) []byte {
	return []byte(fmt.Sprintf(
		"passdb {\n  driver = passwd-file\n  args = /etc/dovecot/apppw/%s.users\n"+
			"  result_failure = continue\n  result_internalfail = continue\n}\n",
		file))
}

// syncMailSubmissionCredential mints the credential a tenant realm authenticates
// with, so that relaying stops depending on where the sender connects from.
//
// Both halves again, and for the same reason as the app passwords: the hash
// where Dovecot verifies it, the plaintext where the consumer reads it. The
// consumer here is the SMTP configuration Job, which runs in the kernel
// namespace, so the plaintext goes there rather than into the tenant's.
func (r *TenantReconciler) syncMailSubmissionCredential(ctx context.Context, tenant *gentianov1alpha1.Tenant, domain string, seed []byte) error {
	addr := mailSubmissionLocalPart + "@" + domain
	pw := deriveMailPassword(seed, mailSubmissionApp, addr)
	// Same verify-before-write as every other identity: a random salt makes an
	// unchanged credential render differently each time, and writing that turns
	// each reconcile into the cause of the next one.
	if err := r.registerSubmissionIdentity(ctx, mailSubmissionApp, tenant.Name, addr, pw); err != nil {
		return err
	}
	return r.upsertSecret(ctx, mailSubmissionSecretPrefix+tenant.Name, defaultServicesNamespace(), map[string][]byte{
		"smtp_user":     []byte(addr),
		"smtp_password": []byte(pw),
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

// argon2idLineMatches reports whether an existing passwd-file line already
// verifies this password.
//
// Needed because the salt is random, so re-rendering a line for an unchanged
// password produces a DIFFERENT string every time. Writing that unconditionally
// makes each reconcile an Update, every Update wakes every watcher, and the
// reconcile that follows writes again — a loop that never converges and shows up
// as unrelated timeouts long before anyone suspects the mail code. Comparing the
// rendered bytes cannot work here; verifying the password is the only way to ask
// "is this already correct?".
//
// A line that cannot be parsed reports false, so a malformed or foreign entry is
// replaced rather than trusted.
func argon2idLineMatches(line, address, password string) bool {
	_, encoded, ok := strings.Cut(strings.TrimSpace(line), ":")
	if !ok || !strings.HasPrefix(encoded, "{ARGON2ID}$argon2id$v=19$") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(encoded, "{ARGON2ID}$argon2id$v=19$"), "$")
	if len(parts) != 3 {
		return false
	}
	var m, t uint32
	var par uint8
	if _, err := fmt.Sscanf(parts[0], "m=%d,t=%d,p=%d", &m, &t, &par); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, par, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// deleteSecretKeys removes keys that are no longer written, leaving the Secret
// and every other key in place.
//
// Needed because upsertSecret merges: a key that stops being produced is not
// overwritten by its absence, it simply stays. For a passwd-file mounted by
// glob that means the credentials in it keep working, which is the opposite of
// what removing them was for.
func (r *TenantReconciler) deleteSecretKeys(ctx context.Context, name, ns string, keys ...string) error {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sec); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	changed := false
	for _, k := range keys {
		if _, ok := sec.Data[k]; ok {
			delete(sec.Data, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Update(ctx, sec)
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
