/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Linked asks the registrar who is linked to the service: the people it acts
// for. It presents the key the operator gave the component, which opens the
// links to this component in its own tenant and nothing else.
type Linked struct {
	Registrar string // REGISTRAR_URL
	Key       string // the component's key
	Tenant    string
	Profile   string
	Client    *http.Client
}

// People are the ids of the people linked to the service.
func (l *Linked) People(ctx context.Context) ([]string, error) {
	target := strings.TrimSuffix(l.Registrar, "/") + "/v1/tenants/" + url.PathEscape(l.Tenant) +
		"/vouching/" + url.PathEscape(l.Profile) + "/people"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.Key)
	client := l.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the registrar answered %d", resp.StatusCode)
	}
	var answer struct {
		People []string `json:"people"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, err
	}
	return answer.People, nil
}
