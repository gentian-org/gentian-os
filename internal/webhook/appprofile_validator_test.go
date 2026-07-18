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

package webhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestAppProfileValidator_Validate(t *testing.T) {
	tests := []struct {
		name       string
		categories []string
		allowed    bool
		wantErr    string
	}{
		{
			name:       "valid categories",
			categories: []string{"Office", "Productivity"},
			allowed:    true,
		},
		{
			name:       "invalid category",
			categories: []string{"Office", "NotARealCategory"},
			allowed:    false,
			wantErr:    "invalid category \"NotARealCategory\"",
		},
		{
			name:       "empty categories",
			categories: nil,
			allowed:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &AppProfileValidator{}
			ap := &gentianov1alpha1.AppProfile{
				Spec: gentianov1alpha1.AppProfileSpec{
					Categories: tt.categories,
				},
			}

			err := validator.Validate(context.Background(), ap)
			if tt.allowed && err != nil {
				t.Fatalf("expected allowed, got error: %v", err)
			}
			if !tt.allowed {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			}
		})
	}
}

func TestAppProfileValidatorHandle_NilReceiver(t *testing.T) {
	var validator *AppProfileValidator
	resp := validator.Handle(context.Background(), admission.Request{})
	if resp.Allowed {
		t.Fatalf("expected request to be rejected")
	}
	if resp.Result == nil || resp.Result.Code != 500 {
		t.Fatalf("expected HTTP 500 response, got: %#v", resp.Result)
	}
	if !strings.Contains(resp.Result.Message, "appprofile validator is nil") {
		t.Fatalf("unexpected message: %q", resp.Result.Message)
	}
}

func TestAppProfileValidatorHandle_InvalidJSON(t *testing.T) {
	validator := &AppProfileValidator{}
	resp := validator.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: []byte("invalid-json")},
		},
	})
	if resp.Allowed {
		t.Fatalf("expected request to be rejected")
	}
	if resp.Result == nil || resp.Result.Code != 400 {
		t.Fatalf("expected HTTP 400 response, got: %#v", resp.Result)
	}
}
