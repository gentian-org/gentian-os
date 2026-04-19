// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import "testing"

func TestSpliceHairpinHosts_NoHostsBlock(t *testing.T) {
	corefile := ".:53 {\n    errors\n    ready\n    kubernetes cluster.local in-addr.arpa ip6.arpa {\n      pods insecure\n      fallthrough in-addr.arpa ip6.arpa\n    }\n    forward . /etc/resolv.conf\n    cache 30\n}\n"

	desired := "# BEGIN gentian-hairpin\n          10.0.0.1 files.example.com\n          10.0.0.1 office.example.com\n          # END gentian-hairpin"

	result := spliceHairpinHosts(corefile, desired)

	if result == corefile {
		t.Fatal("expected Corefile to be modified")
	}
	if !containsAll(result, "hosts {", "# BEGIN gentian-hairpin", "files.example.com",
		"office.example.com", "# END gentian-hairpin", "fallthrough", "kubernetes cluster.local") {
		t.Errorf("unexpected result:\n%s", result)
	}
	hostsIdx := indexOf(result, "hosts {")
	kubeIdx := indexOf(result, "kubernetes cluster.local")
	if hostsIdx >= kubeIdx {
		t.Errorf("hosts block should appear before kubernetes directive")
	}
}

func TestSpliceHairpinHosts_ExistingHostsBlock(t *testing.T) {
	corefile := ".:53 {\n    errors\n    hosts {\n      192.168.0.100 manual.example.com\n      fallthrough\n    }\n    kubernetes cluster.local in-addr.arpa ip6.arpa {\n      pods insecure\n    }\n}\n"

	desired := "# BEGIN gentian-hairpin\n          10.0.0.1 files.example.com\n          # END gentian-hairpin"

	result := spliceHairpinHosts(corefile, desired)

	if !containsAll(result, "# BEGIN gentian-hairpin", "files.example.com",
		"# END gentian-hairpin", "fallthrough", "manual.example.com") {
		t.Errorf("unexpected result:\n%s", result)
	}
}

func TestSpliceHairpinHosts_ReplaceSentinels(t *testing.T) {
	corefile := ".:53 {\n    errors\n    hosts {\n      # BEGIN gentian-hairpin\n          10.0.0.1 old.example.com\n          # END gentian-hairpin\n      fallthrough\n    }\n    kubernetes cluster.local {\n      pods insecure\n    }\n}\n"

	desired := "# BEGIN gentian-hairpin\n          10.0.0.2 new.example.com\n          10.0.0.2 other.example.com\n          # END gentian-hairpin"

	result := spliceHairpinHosts(corefile, desired)

	if containsStr(result, "old.example.com") {
		t.Error("old entries should be removed")
	}
	if !containsAll(result, "new.example.com", "other.example.com", "10.0.0.2") {
		t.Errorf("new entries missing:\n%s", result)
	}
	if countStr(result, "# BEGIN gentian-hairpin") != 1 {
		t.Error("expected exactly one BEGIN sentinel")
	}
	if countStr(result, "# END gentian-hairpin") != 1 {
		t.Error("expected exactly one END sentinel")
	}
}

func TestSpliceHairpinHosts_EmptyTenants(t *testing.T) {
	corefile := ".:53 {\n    errors\n    hosts {\n      # BEGIN gentian-hairpin\n          10.0.0.1 old.example.com\n          # END gentian-hairpin\n      fallthrough\n    }\n    kubernetes cluster.local {\n      pods insecure\n    }\n}\n"

	desired := "# BEGIN gentian-hairpin\n          # END gentian-hairpin"

	result := spliceHairpinHosts(corefile, desired)

	if containsStr(result, "old.example.com") {
		t.Error("old entries should be removed")
	}
	if !containsAll(result, "# BEGIN gentian-hairpin", "# END gentian-hairpin") {
		t.Errorf("sentinels missing:\n%s", result)
	}
}

func TestSpliceHairpinHosts_HostsBlockNoFallthrough(t *testing.T) {
	corefile := ".:53 {\n    hosts {\n      192.168.0.1 manual.local\n    }\n    kubernetes cluster.local {\n      pods insecure\n    }\n}\n"

	desired := "# BEGIN gentian-hairpin\n          10.0.0.1 app.example.com\n          # END gentian-hairpin"

	result := spliceHairpinHosts(corefile, desired)

	if !containsAll(result, "# BEGIN gentian-hairpin", "app.example.com",
		"fallthrough", "manual.local") {
		t.Errorf("unexpected result:\n%s", result)
	}
}

func TestFindMatchingBrace(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		open   int
		expect int
	}{
		{"simple", "{ }", 0, 2},
		{"nested", "{ { } }", 0, 6},
		{"deep", "{ { { } } }", 0, 10},
		{"no match", "{ {", 0, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findMatchingBrace(tt.input, tt.open)
			if got != tt.expect {
				t.Errorf("findMatchingBrace(%q, %d) = %d, want %d", tt.input, tt.open, got, tt.expect)
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !containsStr(s, sub) {
			return false
		}
	}
	return true
}

func containsStr(s, sub string) bool {
	return indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func countStr(s, sub string) int {
	n := 0
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			n++
			i += len(sub) - 1
		}
	}
	return n
}
