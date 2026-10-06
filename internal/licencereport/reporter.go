/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package licencereport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Reporter sends the report shortly after start and once a day after that.
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
}

// Defaults for the schedule.
const (
	DefaultInitialDelay = 3 * time.Minute
	DefaultInterval     = 24 * time.Hour
	DefaultRetryMin     = time.Minute
	DefaultRetryMax     = time.Hour
)

// NeedLeaderElection keeps one replica reporting: two would send two reports
// and race for one sequence.
func (r *Reporter) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It returns only when ctx ends.
func (r *Reporter) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("licence-report")
	if !r.Settings.Active() {
		// Said once, and then nothing: no timer, no request, no record.
		logger.Info("licence reporting is off; nothing is sent", "enabled", r.Settings.Enabled, "hasURL", r.Settings.URL != "")
		<-ctx.Done()
		return nil
	}
	logger.Info("licence reporting is on", "url", r.Settings.URL, "firstIn", r.initialDelay(), "every", r.interval())

	wait, retry := r.initialDelay(), time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		next := r.interval()
		// What the next wait would be if this attempt fails, decided before
		// it is made so the record can say when the next one is.
		retry = r.backoff(retry)
		if r.RunOnce(ctx, next, retry) {
			retry = 0
			wait = next
		} else {
			wait = retry
		}
	}
}

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
	logger := log.FromContext(ctx).WithName("licence-report")
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
		logger.Error(err, "the licence report's record cannot be read; nothing was sent")
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
		logger.Info("no licence report was sent: this cluster has no usable signing key", "reason", reason)
		rec.Attempt = failed(reason, err)
		r.save(ctx, rec)
		return false
	}

	tenants, err := Build(ctx, r.Client, r.Counter, r.Identity, func(what string, err error) {
		logger.Info("a count is sent as null: it could not be had", "what", what, "error", err.Error())
	})
	if err != nil {
		logger.Error(err, "no licence report was sent: the cluster's tenants could not be read")
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
		logger.Error(err, "the licence report could not be recorded, so it was not sent")
		return false
	}

	status, err := r.post(ctx, body, signature, key.ID())
	accepted := err == nil && status >= 200 && status < 300
	attempt := &Attempt{At: at.Format(time.RFC3339), HTTPStatus: status}
	switch {
	case accepted:
		attempt.Outcome = OutcomeAccepted
		attempt.NextAt = at.Add(onSuccess).Format(time.RFC3339)
		logger.Info("licence report sent", "sequence", sequence, "status", status)
	case err != nil:
		attempt.Outcome, attempt.Reason, attempt.Error = OutcomeFailed, ReasonUnreachable, err.Error()
		attempt.NextAt = at.Add(onFailure).Format(time.RFC3339)
		logger.Info("the licence report did not arrive; it is tried again", "sequence", sequence, "error", err.Error())
	default:
		attempt.Outcome, attempt.Reason = OutcomeFailed, ReasonRefused
		attempt.Error = fmt.Sprintf("the receiving endpoint answered %d", status)
		attempt.NextAt = at.Add(onFailure).Format(time.RFC3339)
		logger.Info("the licence report was not accepted; it is tried again", "sequence", sequence, "status", status)
	}
	rec.Attempt = attempt
	r.save(ctx, rec)
	return accepted
}

// save writes the record and only logs a failure to: the attempt it describes
// has already happened.
func (r *Reporter) save(ctx context.Context, rec *Record) {
	if err := writeRecord(ctx, r.Client, r.Reader, r.Namespace, rec); err != nil {
		log.FromContext(ctx).WithName("licence-report").Error(err, "the licence report's outcome could not be recorded")
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
