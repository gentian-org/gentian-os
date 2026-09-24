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

package gitops

import "testing"

// The parse is where this can go quietly wrong: git writes the file names
// after the format, so a record boundary in the wrong place silently drops
// every commit but one — which is exactly what it did first.
func TestEveryCommitIsParsedWithItsOwnFiles(t *testing.T) {
	out := "\x1eaaa111\x1fTom\x1ftom@example.com\x1f2026-09-24T10:00:00+02:00\x1fSet the backup policy for tenant demo\x1freq=abc123 user:tom can_set_policy tenant:demo allowed\n" +
		"\nclusters/c/tenants/demo/backup-policy.yaml\nclusters/c/tenants/demo/kustomization.yaml\n\n" +
		"\x1ebbb222\x1fbrandli\x1fch@example.com\x1f2026-09-23T09:00:00+02:00\x1fseed the tenant\x1f\n" +
		"clusters/c/tenants/demo/tenant.yaml\n"

	changes := parseChanges(out)
	if len(changes) != 2 {
		t.Fatalf("parsed %d commits, expected 2: %+v", len(changes), changes)
	}

	// The one the director made carries its authority, and its own files.
	first := changes[0]
	if first.Commit != "aaa111" || first.Author.Name != "Tom" || first.Summary != "Set the backup policy for tenant demo" {
		t.Fatalf("fields: %+v", first)
	}
	if !first.ThroughPlatform || first.Principal != "tom" ||
		first.Decision != "can_set_policy tenant:demo" || first.RequestID != "abc123" {
		t.Fatalf("authority: %+v", first)
	}
	if len(first.Files) != 2 || first.Files[0] != "clusters/c/tenants/demo/backup-policy.yaml" {
		t.Fatalf("files: %v", first.Files)
	}

	// The one pushed by hand says so, rather than borrowing the other's.
	second := changes[1]
	if second.ThroughPlatform || second.Principal != "" || second.Decision != "" {
		t.Fatalf("a hand-pushed commit claims an authority: %+v", second)
	}
	if len(second.Files) != 1 || second.Files[0] != "clusters/c/tenants/demo/tenant.yaml" {
		t.Fatalf("files: %v", second.Files)
	}
}

// A trailer that is not the shape this expects must not produce a confident
// wrong answer: what can be read is read, and the rest stays empty.
func TestAMalformedTrailerIsNotGuessedAt(t *testing.T) {
	for _, trailer := range []string{"", "nonsense", "req=only"} {
		change := Change{}
		applyAuthzTrailer(&change, trailer)
		if trailer == "" {
			if change.ThroughPlatform {
				t.Errorf("no trailer read as one")
			}
			continue
		}
		if !change.ThroughPlatform {
			t.Errorf("%q: a trailer is a trailer even when it is malformed", trailer)
		}
		if change.Decision != "" {
			t.Errorf("%q: invented a decision %q", trailer, change.Decision)
		}
	}
}
