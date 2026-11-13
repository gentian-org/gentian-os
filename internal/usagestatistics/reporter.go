/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usagestatistics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Reporter sends the report shortly after start and once a day after that,
// and again soon after the cluster's tenants change: a tenant provisioned
// between two daily reports is otherwise unknown to the receiving end for up
// to a day, and that end looks a tenant up by what the latest report names.
//
// It is a runnable of the operator's manager and nothing else depends on it:
// a report that fails is recorded and tried again, and no failure here is
// ever returned to the manager, because a manager stops when a runnable does.
type Reporter struct {
	// Client lists tenants and writes the record. Reader reads the record
	// uncached, so that the sequence continues from what is stored and not
	// from what a cache has seen.
	Client client.Client
	Reader client.Reader
	// Namespace is the control namespace, where the record is kept.
	Namespace string

	Settings Settings
	Identity Identity
	Counter  Counter
	// Key reads the signing key, on every attempt: the Secret it comes from
	// can arrive after this process has started.
	Key func() (*Key, error)

	// HTTP sends the report. Nil is a bounded client that follows no
	// redirect.
	HTTP *http.Client
	// Now is the clock; nil is the wall clock.
	Now func() time.Time

	// InitialDelay is how long after start the first report goes: long
	// enough for a fresh install to have its tenants, short enough that it
	// reports the day it is installed.
	InitialDelay time.Duration
	// Interval is the time between two reports that arrived.
	Interval time.Duration
	// RetryMin and RetryMax bound the wait after a report that did not: it
	// doubles from the first to the second.
	RetryMin time.Duration
	RetryMax time.Duration
	// CheckEvery is how often the tenants are compared with the ones the
	// last report that arrived named. MinGap is the least time between that
	// report and one sent because they changed, so that many tenants
	// provisioned at once go in a few reports and stay well inside what the
	// receiving end takes from one cluster in an hour.
	CheckEvery time.Duration
	MinGap     time.Duration
}

// Defaults for the schedule.
const (
	DefaultInitialDelay = 3 * time.Minute
	DefaultInterval     = 24 * time.Hour
	DefaultRetryMin     = time.Minute
	DefaultRetryMax     = time.Hour
	DefaultCheckEvery   = time.Minute
	DefaultMinGap       = 3 * time.Minute
)

// NeedLeaderElection keeps one replica reporting: two would send two reports
// and race for one sequence.
func (r *Reporter) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It returns only when ctx ends.
func (r *Reporter) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("usage-statistics")
	if !r.Settings.Active() {
		// Said once, and then nothing: no timer, no request, no record.
		logger.Info("usage statistics are off; nothing is sent", "enabled", r.Settings.Enabled, "hasURL", r.Settings.URL != "")
		<-ctx.Done()
		return nil
	}
	logger.Info("usage statistics are on", "url", r.Settings.URL, "firstIn", r.initialDelay(), "every", r.interval())

	// due is when the scheduled report goes: the first, the daily one, or a
	// retry. Between two, the tenants are checked against the ones the last
	// report that arrived named, and a change makes the report go early. A
	// report that failed is not hurried: its retry carries the change.
	due, retry := time.Now().Add(r.initialDelay()), time.Duration(0)
	var named map[string]string
	var sentAt time.Time
	for {
		wait := min(time.Until(due), r.checkEvery())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if time.Now().Before(due) && !r.changed(ctx, named, sentAt) {
			continue
		}
		// What the report is about to name, read before it is built: a
		// tenant that arrives in between is one more change, seen next time.
		current, err := Names(ctx, r.Client, r.Identity)
		if err != nil {
			current = nil
		}
		next := r.interval()
		// What the next wait would be if this attempt fails, decided before
		// it is made so the record can say when the next one is.
		retry = r.backoff(retry)
		if r.RunOnce(ctx, next, retry) {
			retry, named, sentAt = 0, current, time.Now()
			due = sentAt.Add(next)
		} else {
			named = nil
			due = time.Now().Add(retry)
		}
	}
}

// changed reports whether the tenants are no longer the ones the last report
// that arrived named, and that report is far enough back for another to go.
// Without such a report (none yet, or the last attempt failed) the schedule
// alone decides.
func (r *Reporter) changed(ctx context.Context, named map[string]string, sentAt time.Time) bool {
	if named == nil || time.Since(sentAt) < r.minGap() {
		return false
	}
	current, err := Names(ctx, r.Client, r.Identity)
	if err != nil || maps.Equal(current, named) {
		return false
	}
	log.FromContext(ctx).WithName("usage-statistics").Info("the cluster's tenants changed; the report goes now", "tenants", len(current), "before", len(named))
	return true
}

func (r *Reporter) checkEvery() time.Duration { return orDefault(r.CheckEvery, DefaultCheckEvery) }
func (r *Reporter) minGap() time.Duration     { return orDefault(r.MinGap, DefaultMinGap) }

func (r *Reporter) initialDelay() time.Duration {
	return orDefault(r.InitialDelay, DefaultInitialDelay)
}
func (r *Reporter) interval() time.Duration { return orDefault(r.Interval, DefaultInterval) }

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// backoff is the wait after a failure that follows a wait of last.
func (r *Reporter) backoff(last time.Duration) time.Duration {
	lo, hi := orDefault(r.RetryMin, DefaultRetryMin), orDefault(r.RetryMax, DefaultRetryMax)
	switch {
	case last <= 0:
		return lo
	case last*2 > hi:
		return hi
	}
	return last * 2
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Reporter) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return r.clientWithout()
}

// clientWithout is the default client: bounded, and following no redirect.
func (r *Reporter) clientWithout() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		// A redirect is the endpoint naming another host for the body. The
		// address was fixed at install time; nothing the network says moves
		// it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// RunOnce makes one attempt and records it. It reports whether a report was
// sent and accepted. onSuccess and onFailure are how long until the next
// attempt in either case, for the record.
//
// Nothing is sent that was not recorded first. The record is written with the
// body before the request is made, which is also what reserves the sequence:
// a report that left the cluster and whose outcome could then not be written
// down has still used its number.
func (r *Reporter) RunOnce(ctx context.Context, onSuccess, onFailure time.Duration) bool {
	logger := log.FromContext(ctx).WithName("usage-statistics")
	if !r.Settings.Active() {
		return false
	}
	at := r.now()
	failed := func(reason string, err error) *Attempt {
		return &Attempt{
			At: at.Format(time.RFC3339), Outcome: OutcomeNotSent, Reason: reason,
			Error: err.Error(), NextAt: at.Add(onFailure).Format(time.RFC3339),
		}
	}

	rec, err := readRecord(ctx, r.Reader, r.Namespace)
	if err != nil {
		// Without the record there is no sequence to continue and nowhere to
		// write what was sent, so nothing is.
		logger.Error(err, "the usage statistics report's record cannot be read; nothing was sent")
		return false
	}

	key, err := r.Key()
	if err != nil {
		reason := ReasonKeyInvalid
		if errors.Is(err, ErrNoKey) {
			reason = ReasonKeyAbsent
		}
		// Never unsigned. The condition is recorded where the report would
		// have been, and the key is looked for again on the next attempt.
		logger.Info("no usage statistics report was sent: this cluster has no usable signing key", "reason", reason)
		rec.Attempt = failed(reason, err)
		r.save(ctx, rec)
		return false
	}

	tenants, err := Build(ctx, r.Client, r.Counter, r.Identity, func(what string, err error) {
		logger.Info("a count is sent as null: it could not be had", "what", what, "error", err.Error())
	})
	if err != nil {
		logger.Error(err, "no usage statistics report was sent: the cluster's tenants could not be read")
		rec.Attempt = failed(ReasonNoInventory, err)
		r.save(ctx, rec)
		return false
	}

	sequence := rec.sequence() + 1
	body, err := encode(sequence, at, r.Identity, tenants, key)
	if err != nil {
		rec.Attempt = failed(ReasonNoInventory, err)
		r.save(ctx, rec)
		return false
	}
	signature := key.Sign(body)

	rec.Report = &Sent{Sequence: sequence, Body: string(body), Signature: signature, KeyID: key.ID()}
	rec.Attempt = &Attempt{At: at.Format(time.RFC3339), Outcome: OutcomeSending}
	if err := writeRecord(ctx, r.Client, r.Reader, r.Namespace, rec); err != nil {
		logger.Error(err, "the usage statistics report could not be recorded, so it was not sent")
		return false
	}

	status, err := r.post(ctx, body, signature, key.ID())
	accepted := err == nil && status >= 200 && status < 300
	attempt := &Attempt{At: at.Format(time.RFC3339), HTTPStatus: status}
	switch {
	case accepted:
		attempt.Outcome = OutcomeAccepted
		attempt.NextAt = at.Add(onSuccess).Format(time.RFC3339)
		logger.Info("usage statistics report sent", "sequence", sequence, "status", status)
	case err != nil:
		attempt.Outcome, attempt.Reason, attempt.Error = OutcomeFailed, ReasonUnreachable, err.Error()
		attempt.NextAt = at.Add(onFailure).Format(time.RFC3339)
		logger.Info("the usage statistics report did not arrive; it is tried again", "sequence", sequence, "error", err.Error())
	default:
		attempt.Outcome, attempt.Reason = OutcomeFailed, ReasonRefused
		attempt.Error = fmt.Sprintf("the receiving endpoint answered %d", status)
		attempt.NextAt = at.Add(onFailure).Format(time.RFC3339)
		logger.Info("the usage statistics report was not accepted; it is tried again", "sequence", sequence, "status", status)
	}
	rec.Attempt = attempt
	r.save(ctx, rec)
	return accepted
}

// save writes the record and only logs a failure to: the attempt it describes
// has already happened.
func (r *Reporter) save(ctx context.Context, rec *Record) {
	if err := writeRecord(ctx, r.Client, r.Reader, r.Namespace, rec); err != nil {
		log.FromContext(ctx).WithName("usage-statistics").Error(err, "the usage statistics report's outcome could not be recorded")
	}
}

// post sends the body and answers the status. The answer's own body is
// discarded unread past a bound: nothing the endpoint says is acted on.
func (r *Reporter) post(ctx context.Context, body []byte, signature, keyID string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Settings.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, signature)
	req.Header.Set(KeyIDHeader, keyID)
	resp, err := r.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}
