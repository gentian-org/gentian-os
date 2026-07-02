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
