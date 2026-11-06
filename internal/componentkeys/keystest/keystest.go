/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package keystest records what a service does with the operator's list of
// component keys -- which lists it reads and how it refuses the others,
// which key it finds whose entry, and what it logs while it follows the
// file -- so that a test can hold all of it to a transcript.
package keystest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/guard/guardtest"
)

var update = flag.Bool("update-keys", false, "rewrite the recorded key transcripts")

// Hash is the hex SHA-256 of a key, as the operator lists it.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Lists are the lists every reader is held to: good ones and each way of
// being refused. entry is how the reader's entry is written from a tenant,
// a component and a key hash, with whatever else it carries.
func Lists(entry func(tenant, component, hash string) string) [][2]string {
	list := func(entries ...string) string { return `{"keys":[` + strings.Join(entries, ",") + `]}` }
	return [][2]string{
		{"not a list", `nonsense`},
		{"no keys", `{}`},
		{"an empty list", list()},
		{"one", list(entry("demo", "notary", Hash("notary-key")))},
		{"two", list(entry("demo", "notary", Hash("notary-key")), entry("solo", "scribe", Hash("scribe-key")))},
		{"no tenant", list(entry("", "notary", Hash("a")))},
		{"a blank tenant", list(entry(" ", "notary", Hash("a")))},
		{"no component", list(entry("demo", "", Hash("a")))},
		{"the key itself", list(entry("demo", "notary", "notary-key"))},
		{"an upper-case hash", list(entry("demo", "notary", strings.ToUpper(Hash("a"))))},
		{"a short hash", list(entry("demo", "notary", Hash("a")[:63]))},
		{"a shared key", list(entry("demo", "notary", Hash("a")), entry("solo", "notary", Hash("a")))},
		{"a bad second entry", list(entry("demo", "notary", Hash("a")), entry("solo", "", Hash("b")))},
	}
}

// Keys are the keys a reader is asked to find.
var Keys = []string{"", "notary-key", "scribe-key", Hash("notary-key"), "other"}

// Golden holds lines to testdata/name.txt, or writes it with -update-keys.
func Golden(t *testing.T, name string, lines []string) {
	t.Helper()
	path := filepath.Join("testdata", name+".txt")
	got := strings.Join(lines, "\n") + "\n"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // A test fixture.
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path) //nolint:gosec // A test fixture.
	if err != nil {
		t.Fatalf("%v (run with -update-keys to record)", err)
	}
	if string(raw) != got {
		t.Fatalf("the transcript differs from %s (run with -update-keys to record)\n--- got\n%s--- want\n%s", path, got, raw)
	}
}

// Follow drives a follower through the life of its file -- none, a list, one
// that cannot be read, the same list written again, the file gone, a list
// again -- and records, after each step, what it logged and what probe says
// it holds.
func Follow(t *testing.T, follow func(ctx context.Context, path string, every time.Duration, log *slog.Logger),
	entry func(tenant, component, hash string) string, probe func() string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	when := time.Now()
	write := func(content string) {
		t.Helper()
		tmp := path + ".next"
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		// A modification time of its own for each write, whatever the
		// clock's grain.
		when = when.Add(time.Second)
		if err := os.Chtimes(tmp, when, when); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	logs := &guardtest.Logs{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go follow(ctx, path, 5*time.Millisecond, logs.Logger())

	var out []string
	last := ""
	// A step waits for the line it is taken by (none for a quiet one), and
	// a little longer for what follows it. A line the follower logs on every
	// poll -- one left over from the step before, or one repeated -- is
	// recorded once.
	step := func(name, want string) {
		t.Helper()
		var got []string
		deadline := time.Now().Add(5 * time.Second)
		for want != "" && !slices.ContainsFunc(got, func(l string) bool { return strings.Contains(l, want) }) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: nothing logged %q; logged %q", name, want, got)
			}
			time.Sleep(5 * time.Millisecond)
			got = append(got, logs.Take()...)
		}
		time.Sleep(50 * time.Millisecond)
		got = append(got, logs.Take()...)
		out = append(out, "# "+name)
		for _, l := range got {
			if l == last {
				continue
			}
			last = l
			out = append(out, "log "+strings.ReplaceAll(l, path, "<path>"))
		}
		out = append(out, "holds "+probe())
	}
	loaded, refused, gone := `keys loaded"`, "WARN", "are gone"
	two := `{"keys":[` + entry("demo", "notary", Hash("notary-key")) + "," + entry("solo", "scribe", Hash("scribe-key")) + `]}`
	step("no file", "")
	write(two)
	step("a list", loaded)
	write(`{"keys": this is not a list}`)
	step("a file that cannot be read", refused)
	write(two)
	step("the list again", loaded)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	step("the file gone", gone)
	step("still gone", "")
	write(`{"keys":[` + entry("demo", "notary", Hash("notary-key")) + `]}`)
	step("a list again", loaded)
	return out
}

// Error is an error as a transcript line.
func Error(err error) string {
	if err == nil {
		return "ok"
	}
	return fmt.Sprintf("refused: %v", err)
}
