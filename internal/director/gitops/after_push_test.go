/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// Whoever is to be told of a push is told once for each commit that reached
// the remote, with who it was -- and not for a write that changed nothing,
// nor for a push the remote refused.
func TestWhoeverListensIsToldOncePerPushThatLanded(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	var told []string
	g.AfterPush(func(actor string) { told = append(told, actor) })
	ctx := context.Background()

	if _, err := g.Install(ctx, "demo", "element", "", meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ada@example.com"}; !reflect.DeepEqual(told, want) {
		t.Fatalf("after one push: told %v, want %v", told, want)
	}

	// Already so: no commit, no push, nobody told.
	if res, err := g.Install(ctx, "demo", "element", "", meta("u-ada")); err != nil || res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(told) != 1 {
		t.Fatalf("a write that changed nothing told somebody: %v", told)
	}

	// Refused by the remote: the commit is not in the repository.
	hook := remote + "/hooks/pre-receive"
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused by policy >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Install(ctx, "demo", "jitsi", "", meta("u-ada")); err == nil {
		t.Fatal("install succeeded against a remote that refuses pushes")
	}
	if len(told) != 1 {
		t.Fatalf("a push that did not land told somebody: %v", told)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}

	// One file or several, a commit is one push.
	if _, err := g.Uninstall(ctx, "demo", "element", meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	if len(told) != 2 {
		t.Fatalf("after two pushes: told %v", told)
	}
}

// The director tells the operator of a push so that Argo CD looks at once.
// The commit is what was asked for and it is made: an operator that fails,
// refuses or is not there changes nothing about the answer, and neither does
// one that is slow.
func TestAnOperatorThatCannotBeToldDoesNotFailOrDelayTheWrite(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	asked := 0
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer slow.Close()
	defer close(release)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	for name, operator := range map[string]string{"slow and failing": slow.URL, "not there": gone.URL} {
		t.Run(name, func(t *testing.T) {
			remote := dt.Remote(t, "demo")
			g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
			ended := make(chan error, 1)
			client := lifecycle.New(operator, func() string { return "director" })
			g.AfterPush(client.AfterPush(nil, func(err error) { ended <- err }))

			res, err := g.Install(context.Background(), "demo", "element", "", meta("u-ada"))
			if err != nil || !res.Changed {
				t.Fatalf("the write failed with its operator %s: res=%+v err=%v", name, res, err)
			}
			if tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); tip != res.Commit {
				t.Fatalf("result names %s, remote is at %s", res.Commit, tip)
			}
			// The write has answered. Whether the operator has is a
			// separate matter, and for the slow one it has not.
			if operator == slow.URL {
				select {
				case err := <-ended:
					t.Fatalf("the write waited for the operator, which answered %v", err)
				default:
				}
				return
			}
			select {
			case err := <-ended:
				if err == nil {
					t.Fatal("an operator that is not there was reported as told")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("telling an operator that is not there never ended")
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if asked > 1 {
		t.Fatalf("one push asked the operator %d times", asked)
	}
}
