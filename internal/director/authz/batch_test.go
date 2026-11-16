/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/openfga/openfgatest"
)

// results is a batch-check answer for n checks, allowed where allow says.
func results(n int, allow func(i int) bool) string {
	r := map[string]any{}
	for i := range n {
		r[fmt.Sprint(i)] = map[string]any{"allowed": allow(i)}
	}
	b, _ := json.Marshal(map[string]any{"result": r})
	return string(b)
}

func batchClient(t *testing.T, replies ...openfgatest.Reply) (*OpenFGA, *openfgatest.Recorder) {
	t.Helper()
	rec := openfgatest.New(t, replies...)
	c, err := NewOpenFGA(Options{
		BaseURL: rec.URL, StoreID: "s1", ModelID: "m1",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

// More questions than one request may carry go in several, and the answers
// come back in the order asked.
func TestABatchIsSplitAtOpenFGAsLimitAndAnsweredInOrder(t *testing.T) {
	even := func(i int) bool { return i%2 == 0 }
	c, rec := batchClient(t,
		openfgatest.OK(results(50, even)), openfgatest.OK(results(50, even)), openfgatest.OK(results(20, even)))
	var qs []Question
	for i := range 120 {
		qs = append(qs, Question{User: fmt.Sprintf("user:u%d", i), Relation: "can_launch", Object: "app:acme/notes"})
	}
	got, err := c.BatchCheck(context.Background(), "req-1", qs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 120 {
		t.Fatalf("%d answers for 120 questions", len(got))
	}
	for i, ok := range got {
		if ok != even(i%50) {
			t.Errorf("answer %d is %v", i, ok)
		}
	}
	reqs := rec.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	for i, want := range []int{50, 50, 20} {
		if reqs[i].URI != "/stores/s1/batch-check" {
			t.Errorf("request %d went to %s", i, reqs[i].URI)
		}
		var body struct {
			Checks []struct {
				TupleKey      map[string]string `json:"tuple_key"`
				CorrelationID string            `json:"correlation_id"`
			} `json:"checks"`
			Model string `json:"authorization_model_id"`
		}
		if err := json.Unmarshal([]byte(reqs[i].Body), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Checks) != want || body.Model != "m1" {
			t.Errorf("request %d: %d checks against %q, want %d against m1", i, len(body.Checks), body.Model, want)
		}
	}
}

// A check OpenFGA could not answer, or did not answer at all, fails the
// whole batch: it never reads as a denial, nor as an answer to the rest.
func TestAnUnansweredCheckFailsTheBatch(t *testing.T) {
	qs := []Question{
		{User: "user:a", Relation: "can_launch", Object: "app:acme/notes"},
		{User: "user:b", Relation: "can_launch", Object: "app:acme/notes"},
	}
	for name, body := range map[string]string{
		"an error":  `{"result":{"0":{"allowed":true},"1":{"error":{"message":"timeout"}}}}`,
		"no answer": `{"result":{"0":{"allowed":true}}}`,
	} {
		c, _ := batchClient(t, openfgatest.OK(body))
		if got, err := c.BatchCheck(context.Background(), "req-1", qs); err == nil {
			t.Errorf("%s: answered %v", name, got)
		}
	}
	c, _ := batchClient(t, openfgatest.Reply{Status: 500, Body: `{"message":"down"}`})
	if _, err := c.BatchCheck(context.Background(), "req-1", qs); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a refused request: %v", err)
	}
}

// singleOnly is a Checker that cannot batch.
type singleOnly map[string]bool

func (s singleOnly) Check(_ context.Context, _, user, relation, object string) (bool, error) {
	if user == "user:broken" {
		return false, fmt.Errorf("unreachable")
	}
	return s[user+" "+relation+" "+object], nil
}

// A checker that cannot batch is asked one question at a time, with the
// same answer, and an error answers nothing.
func TestCheckAllAsksOneAtATimeWhereItCannotBatch(t *testing.T) {
	c := singleOnly{"user:a can_launch app:x": true}
	got, err := CheckAll(context.Background(), c, "req-1", []Question{
		{"user:a", "can_launch", "app:x"}, {"user:b", "can_launch", "app:x"},
	})
	if err != nil || len(got) != 2 || !got[0] || got[1] {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := CheckAll(context.Background(), c, "req-1", []Question{
		{"user:a", "can_launch", "app:x"}, {"user:broken", "can_launch", "app:x"},
	}); err == nil {
		t.Errorf("an error answered %v", got)
	}
}
