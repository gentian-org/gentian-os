/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package licencereport

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The record: what was last sent, and what became of the last attempt.
//
// A ConfigMap in the control namespace, and not a Secret, because nothing in
// it is one: the body is what the cluster told somebody else, and the
// signature and the key's id are what went with it. It is there so that
// whoever runs the cluster can read what the cluster said about them.

const (
	// RecordConfigMap is the ConfigMap the record is kept in.
	RecordConfigMap = "gentian-licence-report"

	keySequence  = "sequence"
	keyBody      = "body"
	keySignature = "signature"
	keyKeyID     = "keyId"
	keyAttempt   = "attempt"
)

// Outcomes of an attempt.
const (
	// OutcomeAccepted is a report the endpoint answered with a 2xx.
	OutcomeAccepted = "accepted"
	// OutcomeFailed is a report that was sent and did not arrive or was
	// refused. It is tried again.
	OutcomeFailed = "failed"
	// OutcomeNotSent is an attempt that sent nothing, for the Reason given.
	OutcomeNotSent = "not-sent"
	// OutcomeSending is a report recorded and on its way: what a reader sees
	// between the two writes, or after a process that died between them.
	OutcomeSending = "sending"
)

// Reasons an attempt did not end in an accepted report.
const (
	ReasonKeyAbsent   = "signing-key-absent"
	ReasonKeyInvalid  = "signing-key-invalid"
	ReasonNoInventory = "inventory-unavailable"
	ReasonUnreachable = "endpoint-unreachable"
	ReasonRefused     = "endpoint-refused"
)

// Attempt is the last time a report was due.
type Attempt struct {
	At      string `json:"at"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// HTTPStatus is the endpoint's answer, when it gave one.
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Error      string `json:"error,omitempty"`
	// NextAt is when the next attempt is made.
	NextAt string `json:"nextAt,omitempty"`
}

// Sent is the last report put on the wire, exactly.
type Sent struct {
	Sequence int64 `json:"sequence"`
	// Body is the request body byte for byte, as a string: it is what the
	// signature is over, so it is not re-rendered for whoever reads it.
	Body string `json:"body"`
	// Signature and KeyID are the two headers that went with it.
	Signature string `json:"signature"`
	KeyID     string `json:"keyId"`
}

// Record is both.
type Record struct {
	// Attempt is nil on a cluster that has not tried yet.
	Attempt *Attempt `json:"attempt,omitempty"`
	// Report is nil until a report has been sent. It stays the last one sent
	// through later attempts that sent nothing.
	Report *Sent `json:"report,omitempty"`
}

func (r *Record) sequence() int64 {
	if r.Report == nil {
		return 0
	}
	return r.Report.Sequence
}

// readRecord reads the record; a cluster with none has an empty one.
func readRecord(ctx context.Context, c client.Reader, namespace string) (*Record, error) {
	var cm corev1.ConfigMap
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: RecordConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return &Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s/%s: %w", namespace, RecordConfigMap, err)
	}
	rec := &Record{}
	if raw := cm.Data[keyAttempt]; raw != "" {
		var attempt Attempt
		if err := json.Unmarshal([]byte(raw), &attempt); err == nil {
			rec.Attempt = &attempt
		}
	}
	if raw := cm.Data[keySequence]; raw != "" {
		sequence, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || sequence < 1 {
			// Refused rather than restarted from one: a sequence that went
			// backwards is what a replay looks like to the receiver.
			return nil, fmt.Errorf("%s/%s holds a sequence that is not a number", namespace, RecordConfigMap)
		}
		rec.Report = &Sent{
			Sequence: sequence, Body: cm.Data[keyBody],
			Signature: cm.Data[keySignature], KeyID: cm.Data[keyKeyID],
		}
	}
	return rec, nil
}

// writeRecord stores the record, creating the ConfigMap the first time.
func writeRecord(ctx context.Context, w client.Client, reader client.Reader, namespace string, rec *Record) error {
	data := map[string]string{}
	if rec.Attempt != nil {
		raw, err := json.Marshal(rec.Attempt)
		if err != nil {
			return err
		}
		data[keyAttempt] = string(raw)
	}
	if rec.Report != nil {
		data[keySequence] = strconv.FormatInt(rec.Report.Sequence, 10)
		data[keyBody] = rec.Report.Body
		data[keySignature] = rec.Report.Signature
		data[keyKeyID] = rec.Report.KeyID
	}
	var cm corev1.ConfigMap
	err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: RecordConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return w.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace, Name: RecordConfigMap,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "gentian-os"},
			},
			Data: data,
		})
	}
	if err != nil {
		return err
	}
	cm.Data = data
	return w.Update(ctx, &cm)
}

// View is the answer to "what did this cluster last report": the settings and
// the record, for an operations screen.
type View struct {
	Enabled bool `json:"enabled"`
	// URL is where reports go. Absent when reporting is off.
	URL     string   `json:"url,omitempty"`
	Attempt *Attempt `json:"attempt,omitempty"`
	Report  *Sent    `json:"report,omitempty"`
}

// Read answers the view. A cluster that does not report answers
// {"enabled":false} and reads nothing.
func Read(ctx context.Context, c client.Reader, namespace string, s Settings) (*View, error) {
	if !s.Active() {
		return &View{Enabled: false}, nil
	}
	rec, err := readRecord(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	return &View{Enabled: true, URL: s.URL, Attempt: rec.Attempt, Report: rec.Report}, nil
}
