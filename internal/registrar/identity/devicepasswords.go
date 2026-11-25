/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// A person's device passwords: credentials of the person in the realm, of a
// type the platform's Keycloak extension adds (gentian-device-password). The
// admin API lists them and removes one; making one is the extension's
// endpoint, because Keycloak's API cannot make a credential of a type of its
// own. The secret is never read back: the extension returns it once, when it
// is made.

// DevicePasswordType is the credential type a device password is kept as.
const DevicePasswordType = "gentian-device-password"

// ErrInvalid is a request the realm refused as it was put: a name already
// used, a way that is no way's name, a person who holds too many.
var ErrInvalid = errors.New("invalid request")

// DevicePassword is one of a person's device passwords, without its secret.
type DevicePassword struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Ways       []string   `json:"ways"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// NewDevicePassword is a device password as it is shown once.
type NewDevicePassword struct {
	DevicePassword
	Username string `json:"username"`
	Password string `json:"password"`
}

type credentialRep struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	UserLabel      string `json:"userLabel"`
	CreatedDate    int64  `json:"createdDate"`
	CredentialData string `json:"credentialData"`
}

func devicePasswordOf(c credentialRep) DevicePassword {
	var data struct {
		Ways       []string `json:"ways"`
		LastUsedAt *int64   `json:"lastUsedAt"`
	}
	_ = json.Unmarshal([]byte(c.CredentialData), &data)
	d := DevicePassword{ID: c.ID, Name: c.UserLabel, Ways: data.Ways, CreatedAt: time.UnixMilli(c.CreatedDate).UTC()}
	if d.Ways == nil {
		d.Ways = []string{}
	}
	if data.LastUsedAt != nil {
		t := time.UnixMilli(*data.LastUsedAt).UTC()
		d.LastUsedAt = &t
	}
	return d
}

// DevicePasswords lists a person's device passwords.
func (c *Client) DevicePasswords(ctx context.Context, r Realm, userID string) ([]DevicePassword, error) {
	if !plainID(userID) {
		return nil, fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	var creds []credentialRep
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(userID)+"/credentials", nil, nil, &creds); err != nil {
		return nil, err
	}
	out := []DevicePassword{}
	for _, cr := range creds {
		if cr.Type == DevicePasswordType {
			out = append(out, devicePasswordOf(cr))
		}
	}
	return out, nil
}

// CreateDevicePassword makes a device password for a person, for the named
// ways, and returns it with its secret, which nothing returns again.
func (c *Client) CreateDevicePassword(ctx context.Context, r Realm, userID, name string, ways []string) (NewDevicePassword, error) {
	if !plainID(userID) {
		return NewDevicePassword{}, fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	rel := devicePasswordsPath(r.name, userID)
	label := "/gentian-device-passwords/users/{id}"
	resp, err := c.doAt(ctx, r, http.MethodPost, rel, label, nil, map[string]any{"name": name, "ways": ways})
	if err != nil {
		return NewDevicePassword{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusBadRequest {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return NewDevicePassword{}, fmt.Errorf("%w: %s", ErrInvalid, e.Error)
	}
	if err := statusError(resp.StatusCode, http.MethodPost, label, raw); err != nil {
		return NewDevicePassword{}, err
	}
	var made struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Ways      []string `json:"ways"`
		Username  string   `json:"username"`
		Password  string   `json:"password"`
		CreatedAt int64    `json:"createdAt"`
	}
	if err := json.Unmarshal(raw, &made); err != nil || made.Password == "" {
		return NewDevicePassword{}, fmt.Errorf("keycloak POST %s: no password in the answer", label)
	}
	return NewDevicePassword{
		DevicePassword: DevicePassword{ID: made.ID, Name: made.Name, Ways: made.Ways, CreatedAt: time.UnixMilli(made.CreatedAt).UTC()},
		Username:       made.Username,
		Password:       made.Password,
	}, nil
}

// RevokeDevicePassword removes one of a person's device passwords. A
// credential of the person that is not a device password is not one this
// removes: ErrNotFound, as for one that is not there.
func (c *Client) RevokeDevicePassword(ctx context.Context, r Realm, userID, id string) error {
	if !plainID(userID) || !plainID(id) {
		return fmt.Errorf("%w: device password %q", ErrNotFound, id)
	}
	held, err := c.DevicePasswords(ctx, r, userID)
	if err != nil {
		return err
	}
	for _, d := range held {
		if d.ID == id {
			return c.call(ctx, r, http.MethodDelete, "/users/"+url.PathEscape(userID)+"/credentials/"+url.PathEscape(id), nil, nil, nil)
		}
	}
	return fmt.Errorf("%w: device password %q", ErrNotFound, id)
}

func devicePasswordsPath(realm, userID string) string {
	return "/realms/" + url.PathEscape(realm) + "/gentian-device-passwords/users/" + url.PathEscape(userID)
}
