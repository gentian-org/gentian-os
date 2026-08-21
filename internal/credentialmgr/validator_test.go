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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fieldsOf fails the test unless err is a FieldErrors naming exactly the
// given fields, in any order.
func fieldsOf(t *testing.T, err error, want ...string) {
	t.Helper()
	var fe FieldErrors
	if !errors.As(err, &fe) {
		t.Fatalf("expected a FieldErrors, got %T: %v", err, err)
	}
	got := map[string]bool{}
	for _, f := range fe {
		got[f.Field] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("expected field %q attributed, got %v", w, fe)
		}
	}
	if len(got) != len(want) {
		t.Errorf("expected exactly %v attributed, got %v", want, fe)
	}
}

// TestBasicAuthProbe_RejectionAttributesBothFields covers oci-registry and
// git-https: HTTP basic auth's 401 does not say which half of the pair was
// wrong, so both declared fields are marked suspect rather than one guessed.
func TestBasicAuthProbe_RejectionAttributesBothFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	v := NewEndpointValidator()
	err := v.Validate(context.Background(), "oci-registry", srv.URL,
		map[string]string{"username": "u", "password": "wrong"})
	fieldsOf(t, err, "username", "password")

	// A path segment, not a bare host: git-https appends ".git/info/refs..."
	// directly onto the declared host, the way it would onto a real repo URL
	// like "https://github.com/org/repo".
	err = v.Validate(context.Background(), "git-https", srv.URL+"/repo",
		map[string]string{"username": "u", "token": "wrong"})
	fieldsOf(t, err, "username", "token")
}

// TestBasicAuthProbe_SuccessIsNotAttributed asserts a passing probe carries no
// field error at all — attribution exists only for the failure it names.
func TestBasicAuthProbe_SuccessIsNotAttributed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	v := NewEndpointValidator()
	if err := v.Validate(context.Background(), "oci-registry", srv.URL,
		map[string]string{"username": "u", "password": "correct"}); err != nil {
		t.Fatalf("expected no error on 200, got: %v", err)
	}
}

// TestBasicAuthProbe_UnreachableIsNotAttributed asserts an endpoint problem —
// as opposed to a credential problem — is never blamed on a field. Guessing
// wrong here would send an operator to retype a password that was never the
// issue.
func TestBasicAuthProbe_UnreachableIsNotAttributed(t *testing.T) {
	v := NewEndpointValidator()
	err := v.Validate(context.Background(), "oci-registry", "http://127.0.0.1:1",
		map[string]string{"username": "u", "password": "p"})
	var fe FieldErrors
	if errors.As(err, &fe) {
		t.Fatalf("an unreachable endpoint must not attribute to a field, got: %v", fe)
	}
	if err == nil {
		t.Fatal("expected an error for an unreachable endpoint")
	}
}

// TestBasicAuthProbe_NotFoundIsNotAttributed: a private resource 404s exactly
// like a nonexistent one, so this is not evidence against either field.
func TestBasicAuthProbe_NotFoundIsNotAttributed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	v := NewEndpointValidator()
	err := v.Validate(context.Background(), "git-https", srv.URL,
		map[string]string{"username": "u", "token": "t"})
	var fe FieldErrors
	if errors.As(err, &fe) {
		t.Fatalf("a 404 must not attribute to a field, got: %v", fe)
	}
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
}

// TestBasicAuthProbe_NoHostRefusesToRun covers the requirement declaring a
// validator that needs a host but none in spec.validate.host — the bug this
// closed: the probe used to be handed an always-empty fields["host"], so it
// could never succeed regardless of the credential.
func TestBasicAuthProbe_NoHostRefusesToRun(t *testing.T) {
	v := NewEndpointValidator()
	err := v.Validate(context.Background(), "oci-registry", "",
		map[string]string{"username": "u", "password": "p"})
	if err == nil {
		t.Fatal("expected an error when no host is declared")
	}
	var fe FieldErrors
	if errors.As(err, &fe) {
		t.Fatalf("a missing host is a requirement authoring problem, not a field of the submission: %v", fe)
	}
}

// TestBearerProbe_RejectionAttributesTheOneField covers oidc-discovery, whose
// declared schema has exactly one credential field — unlike basic auth, a
// rejection here is unambiguous.
func TestBearerProbe_RejectionAttributesTheOneField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	v := NewEndpointValidator()
	err := v.Validate(context.Background(), "oidc-discovery", srv.URL,
		map[string]string{"api-token": "wrong"})
	fieldsOf(t, err, "api-token")
}

// TestUnknownValidatorType is an ERROR, not a silent pass — the catalogue
// asked for a check this validator does not implement.
func TestUnknownValidatorType(t *testing.T) {
	v := NewEndpointValidator()
	if err := v.Validate(context.Background(), "made-up", "", nil); err == nil {
		t.Fatal("expected an error for an unrecognised validator type")
	}
}
