// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildGentianGroupsScript_WaitsForRealm(t *testing.T) {
	t.Parallel()
	script := buildGentianGroupsScript("demo")
	if !strings.Contains(script, "not available after waiting") {
		t.Fatal("expected realm wait helper in gentian groups script")
	}
	if strings.Contains(script, "groups?search=") {
		t.Fatal("expected full group list lookup without search query")
	}
	if !strings.Contains(script, "groups?max=1000") {
		t.Fatal("expected paginated group list lookup")
	}
}
