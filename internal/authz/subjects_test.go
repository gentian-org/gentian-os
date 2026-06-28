// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package authz

import "testing"

func TestUserSubject(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"alice", "user:alice"},
		{"user:bob", "user:bob"},
		{"  ", "user:"},
	}
	for _, tc := range tests {
		if got := UserSubject(tc.in); got != tc.want {
			t.Fatalf("UserSubject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestObjectRef(t *testing.T) {
	if got := ObjectRef("tenant", "kernel"); got != "tenant:kernel" {
		t.Fatalf("ObjectRef = %q", got)
	}
}

func TestAuthorizationModelPayload(t *testing.T) {
	schema, defs, err := AuthorizationModelPayload()
	if err != nil {
		t.Fatal(err)
	}
	if schema != "1.1" {
		t.Fatalf("schema = %q", schema)
	}
	if len(defs) == 0 {
		t.Fatal("empty type definitions")
	}
}
