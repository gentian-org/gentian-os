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

package credentialmgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
)

// A tenant's backup identity: the private half of the key its bundles are
// encrypted to, kept so the tenant can restore without the file it downloaded.
//
// Not a CredentialRequirement, deliberately. A requirement is a credential a
// human supplies and something consumes, and its satisfaction is probed by
// External Secrets materialising it into a Secret — which is the one thing that
// must never happen to this value. The requirement machinery would therefore
// have to be defeated in the one respect that makes it useful.
//
// So it is written here instead, by the same rule the rest of this service
// follows: the caller's own exchanged token performs the write, and the path is
// derived from the tenant in the verified claim rather than from anything the
// request says. A tenant admin can escrow into their own subtree and nowhere
// else, and that is a property of OpenBao's policy engine rather than of a check
// in this file.
const (
	// backupIdentityField is the key at that path.
	backupIdentityField = "identity"

	// identityPrefix is what an age private key starts with. Checked so a
	// public key pasted into the wrong half is refused here rather than
	// escrowed as though it were the thing that opens the backups — a mistake
	// whose symptom would otherwise be a failed restore years later.
	identityPrefix = "AGE-SECRET-KEY-"

	// maxIdentityLen bounds the body. An age identity is 74 characters; this
	// leaves room for a trailing newline and for a longer encoding, and refuses
	// a file pasted into the field.
	maxIdentityLen = 256
)

// BackupIdentityPath is where one tenant's escrowed backup identity lives.
//
// Under the tenant's own subtree, so the existing tenant-admin policy already
// reads and writes it and no grant had to be widened. eso-read denies this exact
// path — see the Cluster composition — so escrow means the tenant administrator
// can read the key, not that the cluster can.
func BackupIdentityPath(tenant string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/backup/identity", tenant)
}

type backupIdentityRequest struct {
	Identity string `json:"identity"`
	// Recipient is the public half. Recorded as metadata beside the identity so
	// that "which key is escrowed" can be answered without reading the private
	// one -- a public key is not a secret, and asking which key a workspace
	// uses should not require handling the key that opens its backups.
	Recipient string `json:"recipient"`
}

// recipientPrefix is what an age public key starts with.
const recipientPrefix = "age1"

// handleSetBackupIdentity escrows the caller's tenant backup identity.
func (s *Server) handleSetBackupIdentity(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		s.writeIdentityErr(w, err)
		return
	}

	// The tenant comes from the exchanged identity, never from the request. A
	// name in the body would let any tenant admin escrow a key into another
	// tenant's subtree, and the write would succeed for whichever of them
	// OpenBao's policy happened to allow.
	tenant := c.view.Tenant
	if tenant == "" {
		writeErr(w, http.StatusForbidden,
			errors.New("only a workspace administrator can escrow a backup key"))
		return
	}

	var body backupIdentityRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed body: %w", err))
		return
	}

	identity := strings.TrimSpace(body.Identity)
	switch {
	case identity == "":
		writeErr(w, http.StatusBadRequest, errors.New("identity is required"))
		return
	case len(identity) > maxIdentityLen:
		writeErr(w, http.StatusBadRequest, errors.New("identity is too long to be an age key"))
		return
	case !strings.HasPrefix(identity, identityPrefix):
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("that is not an age private key; they start with %s", identityPrefix))
		return
	}

	recipient := strings.TrimSpace(body.Recipient)
	if recipient != "" && !strings.HasPrefix(recipient, recipientPrefix) {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("that is not an age public key; they start with %s", recipientPrefix))
		return
	}

	path := BackupIdentityPath(tenant)
	fields := map[string]string{backupIdentityField: identity}
	meta := map[string]string{"recipient": recipient}
	if err := s.Bao.WriteWithMetadata(r.Context(), c.bao.Token, path, fields, c.name, meta); err != nil {
		log := ctrl.Log.WithName("credentialmgr")
		if errors.Is(err, ErrUpstream) {
			log.Error(err, "cannot reach OpenBao to escrow this backup key", "tenant", tenant)
			writeErr(w, http.StatusBadGateway,
				errors.New("the credential manager cannot reach OpenBao; the key was not escrowed"))
			return
		}
		log.Error(err, "OpenBao refused the escrow write", "path", path, "setBy", c.name)
		writeErr(w, http.StatusForbidden, err)
		return
	}

	// No refreshConsumers: nothing reads this path on a schedule, and nothing
	// should. It is read by a person performing a restore, once.
	//
	// Metadata only in the response, as everywhere else in this service — an
	// endpoint that echoed the key back would put it in every proxy log between
	// here and the browser.
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":    tenant,
		"vaultPath": path,
		"stored":    true,
		"setBy":     c.name,
		"recipient": recipient,
	})
}

// handleGetBackupIdentity answers whether a workspace has an escrowed backup
// key, and which one -- never the key itself.
//
// Metadata only, and that is not a courtesy: it reads the path's metadata
// endpoint, which does not return the stored value at all. So this cannot leak
// the identity even if it wanted to, and the public half it does return is the
// thing a caller needs in order to encrypt to the existing key.
func (s *Server) handleGetBackupIdentity(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		s.writeIdentityErr(w, err)
		return
	}
	tenant := c.view.Tenant
	if tenant == "" {
		writeErr(w, http.StatusForbidden,
			errors.New("only a workspace administrator has a backup key"))
		return
	}

	md, err := s.Bao.Metadata(r.Context(), c.bao.Token, BackupIdentityPath(tenant))
	if err != nil {
		writeErr(w, http.StatusBadGateway,
			errors.New("the credential manager cannot reach OpenBao"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":    tenant,
		"exists":    md.Exists,
		"recipient": md.Recipient,
		"setBy":     md.SetBy,
		"updatedAt": md.UpdatedAt,
	})
}
