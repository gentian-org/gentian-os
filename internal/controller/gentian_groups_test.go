// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import "testing"

func TestShellWordList_NoShellQuotes(t *testing.T) {
	t.Parallel()
	got := shellWordList([]string{
		"gentian:tenant:demo:members",
		"gentian:tenant:demo:admins",
	})
	want := "gentian:tenant:demo:members gentian:tenant:demo:admins"
	if got != want {
		t.Fatalf("shellWordList() = %q, want %q", got, want)
	}
	if got[0] == '"' || got[len(got)-1] == '"' {
		t.Fatalf("shellWordList() must not add shell quotes, got %q", got)
	}
}
