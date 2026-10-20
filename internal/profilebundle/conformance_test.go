/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/platformversion"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// TestThePublishedCataloguesConform holds every bundle a published catalogue
// serves to the checks this platform runs at install and at rollout: the
// bundle hashes to the digest its index states, it is a bundle this platform
// admits, its profile reads as a ComponentProfile of this build's schema,
// and its platform floor is well-formed.
//
// It runs when GENTIAN_CONFORMANCE_CATALOGUES names the catalogues, comma
// separated -- CI sets it to the released and the develop catalogue -- so a
// change to the platform that breaks a published profile fails here, in the
// platform's own pull request, rather than at a tenant's install. Without
// the variable it is skipped: a unit test does not reach the network.
func TestThePublishedCataloguesConform(t *testing.T) {
	addresses := strings.Split(os.Getenv("GENTIAN_CONFORMANCE_CATALOGUES"), ",")
	if len(addresses) == 1 && strings.TrimSpace(addresses[0]) == "" {
		t.Skip("GENTIAN_CONFORMANCE_CATALOGUES names no catalogue")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, address := range addresses {
		address = strings.TrimSuffix(strings.TrimSpace(address), "/")
		if address == "" {
			continue
		}
		t.Run(address, func(t *testing.T) {
			var index struct {
				Entries []struct {
					Name, Digest, Platform string
				} `json:"entries"`
			}
			body := fetch(t, client, address+"/index.yaml")
			if err := yaml.Unmarshal(body, &index); err != nil {
				t.Fatalf("index.yaml: %v", err)
			}
			if len(index.Entries) == 0 {
				t.Fatal("index.yaml lists nothing")
			}
			for _, e := range index.Entries {
				t.Run(e.Name, func(t *testing.T) {
					bundle := fetch(t, client, address+"/profiles/"+e.Name+".yaml")
					sum := sha256.Sum256(bundle)
					if got := "sha256:" + hex.EncodeToString(sum[:]); got != e.Digest {
						t.Fatalf("the index states %s and the file hashes to %s", e.Digest, got)
					}
					// As a catalogue of the whole cluster brings it: the one origin
					// that may bring companions.
					if _, err := profilebundle.Check(bundle, e.Name, profilebundle.ClusterOrigin("conformance")); err != nil {
						t.Fatalf("not a bundle this platform installs: %v", err)
					}
					profile, err := profilebundle.ReadProfile(bundle)
					if err != nil {
						t.Fatalf("the profile does not read as a ComponentProfile of this platform: %v", err)
					}
					floor := ""
					if profile.Spec.Requires != nil {
						floor = profile.Spec.Requires.Platform
					}
					if _, err := platformversion.ParseFloor(floor); err != nil {
						t.Fatalf("spec.requires.platform: %v", err)
					}
					if floor != e.Platform {
						t.Fatalf("the index states platform %q and the bundle %q", e.Platform, floor)
					}
				})
			}
		})
	}
}

func fetch(t *testing.T, client *http.Client, url string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		t.Fatal(fmt.Errorf("GET %s: %w", url, err))
	}
	return body
}
