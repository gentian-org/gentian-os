/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PlatformRights asks the platform "may this person use this app". The
// platform gives a component that declares requires.services.rights an
// address and a key of its own (the Secret rights-check-<component>). The key
// is good for this one question about this tenant's apps; the chaperone holds
// no credential of the rights store.
type PlatformRights struct {
	url    string
	key    string
	client *http.Client
}

// NewPlatformRights asks at url with key.
func NewPlatformRights(url, key string, client *http.Client) *PlatformRights {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &PlatformRights{url: url, key: key, client: client}
}

func (p *PlatformRights) MayUse(ctx context.Context, person, app string) (bool, error) {
	body, err := json.Marshal(map[string]string{"person": person, "app": app})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.key)
	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("the platform answered %d", resp.StatusCode)
	}
	// Anything but a plain yes is a no.
	var answer struct {
		Allowed any `json:"allowed"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&answer); err != nil {
		return false, err
	}
	return answer.Allowed == true, nil
}
