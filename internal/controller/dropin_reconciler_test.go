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
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func editableDropIn() *gentianov1alpha1.CustomizationDropIn {
	return &gentianov1alpha1.CustomizationDropIn{
		Name:           "branding",
		Path:           "/opt/app/branding",
		Format:         "files",
		TenantEditable: true,
		MaxBytes:       1024,
	}
}

func TestValidateDropInFilesRejectsReservedPrefixes(t *testing.T) {
	// 00-89 belong to platform and profile files. A tenant that could write 10-*
	// would silently outrank the platform's own defaults.
	for _, name := range []string{"10-brand.css", "50-brand.css", "brand.css", "89-brand.css"} {
		err := validateDropInFiles(editableDropIn(), map[string]string{name: "body{}"})
		if err == nil {
			t.Fatalf("expected %q to be rejected", name)
		}
		if !strings.Contains(err.Error(), "reserved for platform and profile") {
			t.Fatalf("unexpected error for %q: %v", name, err)
		}
	}
}

func TestValidateDropInFilesAcceptsTenantRange(t *testing.T) {
	for _, name := range []string{"90-brand.css", "95-theme.css", "99-final.css"} {
		if err := validateDropInFiles(editableDropIn(), map[string]string{name: "body{}"}); err != nil {
			t.Fatalf("expected %q to be accepted, got %v", name, err)
		}
	}
}

func TestValidateDropInFilesEnforcesSizeCap(t *testing.T) {
	declared := editableDropIn()
	declared.MaxBytes = 32
	err := validateDropInFiles(declared, map[string]string{
		"90-brand.css": strings.Repeat("x", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "exceeding the 32 byte limit") {
		t.Fatalf("expected size rejection, got %v", err)
	}
}

func TestValidateDropInFilesParsesDeclaredFormat(t *testing.T) {
	declared := editableDropIn()
	declared.Format = "yaml"
	err := validateDropInFiles(declared, map[string]string{"90-config.yaml": "key: [broken"})
	if err == nil || !strings.Contains(err.Error(), "invalid YAML") {
		t.Fatalf("expected malformed YAML to fail at reconcile, got %v", err)
	}
}

func TestValidateDropInFilesFallsBackToDefaultCap(t *testing.T) {
	declared := editableDropIn()
	declared.MaxBytes = 0
	if err := validateDropInFiles(declared, map[string]string{"90-brand.css": "body{}"}); err != nil {
		t.Fatalf("expected default cap to apply, got %v", err)
	}
}

func TestDropInConfigMapNameIsPredictable(t *testing.T) {
	// The composition mounts by name, so this must stay stable.
	got := dropInConfigMapName("odoo-cb-base", "branding")
	if got != "app-dropin-odoo-cb-base-branding" {
		t.Fatalf("unexpected ConfigMap name %q", got)
	}
}
