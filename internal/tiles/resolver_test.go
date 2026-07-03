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

package tiles

import (
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestResolveIconKnown(t *testing.T) {
	uri, err := ResolveIcon("mail")
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}
	if !strings.HasPrefix(uri, "data:image/svg+xml;base64,") {
		t.Fatalf("unexpected uri prefix: %s", uri[:32])
	}
}

func TestResolveIconUnknown(t *testing.T) {
	if _, err := ResolveIcon("not-a-real-icon"); err == nil {
		t.Fatal("expected error for unknown icon")
	}
}

func TestResolveLogoPriority(t *testing.T) {
	profileIcon, err := ResolveIcon("erp")
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}
	mailIcon, err := ResolveIcon("mail")
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}

	got, err := ResolveLogo(
		&gentianov1alpha1.TileSpec{Icon: "erp"},
		&gentianov1alpha1.TileSpec{Icon: "mail"},
	)
	if err != nil {
		t.Fatalf("ResolveLogo: %v", err)
	}
	if got != mailIcon {
		t.Fatalf("portal tile icon should win")
	}

	got, err = ResolveLogo(&gentianov1alpha1.TileSpec{Icon: "erp"}, nil)
	if err != nil {
		t.Fatalf("ResolveLogo: %v", err)
	}
	if got != profileIcon {
		t.Fatalf("profile tile icon expected")
	}

	got, err = ResolveLogo(nil, nil)
	if err != nil {
		t.Fatalf("ResolveLogo: %v", err)
	}
	defaultIcon, err := ResolveIcon(defaultIconID)
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}
	if got != defaultIcon {
		t.Fatalf("default catalogue icon expected, got %q", got)
	}
}
