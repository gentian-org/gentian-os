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

package identity

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDirectorySourceReadsOneCredentialPerRealm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "demo", "secret-demo\n")
	write(t, dir, "kernel", "secret-kernel")

	s := NewDirectorySource(dir, "gentian-director-admin")
	got := s.Realms()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "demo" || got[1] != "kernel" {
		t.Fatalf("realms = %v", got)
	}
	c, ok := s.For("demo")
	if !ok {
		t.Fatal("no credential for demo")
	}
	// The trailing newline a file may carry is not part of the secret.
	if c.ClientSecret != "secret-demo" || c.ClientID != "gentian-director-admin" || c.TokenRealm != "demo" {
		t.Fatalf("credential = %+v", c)
	}
	if _, ok := s.For("other"); ok {
		t.Fatal("a realm with no file must not resolve")
	}
}

// The reason this re-reads at all: a tenant's realm appears after the director
// started, and a source read once at boot would never speak for it.
func TestDirectorySourcePicksUpARealmAddedLater(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "kernel", "secret-kernel")

	s := NewDirectorySource(dir, "gentian-director-admin")
	s.ttl = 0 // every call re-reads, which is what a real TTL expiry does
	if _, ok := s.For("demo"); ok {
		t.Fatal("demo should not exist yet")
	}
	write(t, dir, "demo", "secret-demo")
	if _, ok := s.For("demo"); !ok {
		t.Fatal("a realm written after start was never picked up")
	}
}

// And the other direction: a credential that was withdrawn stops working.
// Holding the last good set would keep a retired realm reachable.
func TestDirectorySourceForgetsARealmThatWasRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "demo", "secret-demo")

	s := NewDirectorySource(dir, "gentian-director-admin")
	s.ttl = 0
	if _, ok := s.For("demo"); !ok {
		t.Fatal("demo should resolve")
	}
	if err := os.Remove(filepath.Join(dir, "demo")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.For("demo"); ok {
		t.Fatal("a withdrawn credential still resolves")
	}
}

// A projected Secret carries ..data and a timestamped directory beside the
// keys. Treating those as realms would offer a credential named "..data".
func TestDirectorySourceIgnoresTheProjectionsOwnEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "demo", "secret-demo")
	if err := os.Mkdir(filepath.Join(dir, "..2026_09_25_20_00_00.123"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "..data", "not a realm")

	s := NewDirectorySource(dir, "gentian-director-admin")
	got := s.Realms()
	if len(got) != 1 || got[0] != "demo" {
		t.Fatalf("realms = %v, want just demo", got)
	}
}

func TestDirectorySourceWithNoDirectoryIsEmptyRatherThanAnError(t *testing.T) {
	t.Parallel()
	s := NewDirectorySource(filepath.Join(t.TempDir(), "never-written"), "gentian-director-admin")
	if got := s.Realms(); len(got) != 0 {
		t.Fatalf("realms = %v", got)
	}
	if _, ok := s.For("demo"); ok {
		t.Fatal("nothing should resolve")
	}
}

// An empty file is not a credential. It is what a Secret key looks like
// halfway through being written, and offering it would produce a token
// request refused on the secret rather than a realm that is not there yet.
func TestDirectorySourceSkipsAnEmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "demo", "   \n")
	s := NewDirectorySource(dir, "gentian-director-admin")
	if _, ok := s.For("demo"); ok {
		t.Fatal("an empty secret must not resolve")
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
