/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/mailer"
	"github.com/gentian-org/gentian-os/internal/releaseindex"
)

func testReleaseIndex(context.Context) (releaseindex.Index, error) {
	return releaseindex.Index{Releases: []releaseindex.Release{
		{Version: "v0.5.0", Line: "0.5"},
		{Version: "v0.5.1", Line: "0.5", UpgradesFrom: []string{"0.5"}},
		{Version: "v0.6.0", Line: "0.6", Notes: "https://github.com/gentian-org/gentian-os/releases/tag/v0.6.0"},
	}}, nil
}

func notifyFixtures(configured bool) []client.Object {
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "Cluster",
		"metadata": map[string]any{"name": "cluster", "namespace": layout.Namespace(layout.Provisioning)},
		"spec":     map[string]any{"kernelDomain": "example.org", "platformRoles": map[string]any{"admin": "gentian:platform:admin"}},
	}}
	data := map[string][]byte{
		"smtp_host": []byte("mail.example.org"), "smtp_user": []byte("gentian-system@example.org"), "smtp_password": []byte("pw"),
		"smtp_starttls": []byte("true"), "kernel_realm": []byte("kernel"),
	}
	if configured {
		data["smtp_configure"] = []byte("true")
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keycloakSMTPCredentialsSecret, Namespace: layout.Namespace(layout.Authentication)},
		Data:       data,
	}
	return []client.Object{claim, secret}
}

func TestTheAdministratorsAreMailedThroughThePlatformsSettings(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(notifyFixtures(true)...).Build()
	var sent []mailer.Message
	var usedSettings mailer.Settings
	var askedRealm, askedGroup string
	s := &Service{client: c, fetchReleaseIndex: testReleaseIndex,
		sendMail: func(_ context.Context, st mailer.Settings, m mailer.Message) error {
			usedSettings = st
			sent = append(sent, m)
			return nil
		},
		listGroupMembers: func(_ context.Context, realm, group string) ([]authz.KeycloakUser, error) {
			askedRealm, askedGroup = realm, group
			return []authz.KeycloakUser{
				{Username: "administrator", Email: "Ops@Example.org", Enabled: true},
				{Username: "alice@example.org", Enabled: true},
				{Username: "gone@example.org", Enabled: false},
				{Username: "nomail", Enabled: true},
				{Username: "ops@example.org", Enabled: true},
			}, nil
		},
	}
	h := &HTTPServer{Service: s}
	rec := httptest.NewRecorder()
	body := `{"event":"release-pending","release":"0.6.0","from":"v0.5.1"}`
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	var res NotifyAdministratorsResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if strings.Join(res.Recipients, ",") != "alice@example.org,ops@example.org" {
		t.Fatalf("recipients = %v", res.Recipients)
	}
	if askedRealm != "kernel" || askedGroup != "gentian:platform:admin" {
		t.Fatalf("asked %s/%s", askedRealm, askedGroup)
	}
	if len(sent) != 1 || sent[0].Subject != "Gentian OS v0.6.0 waits for cluster example.org" ||
		!strings.Contains(sent[0].Text, "runs v0.5.1") || !strings.Contains(sent[0].Text, "release set v0.6.0\n") ||
		!strings.Contains(sent[0].Text, "Release notes: https://github.com/gentian-org/gentian-os/releases/tag/v0.6.0") {
		t.Fatalf("sent = %+v", sent)
	}
	if usedSettings.Host != "mail.example.org" || usedSettings.From != "noreply@example.org" || !usedSettings.StartTLS {
		t.Fatalf("settings = %+v", usedSettings)
	}
}

func TestWithoutMailOrAddressesTheActionSaysSo(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	// Mail not configured.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(notifyFixtures(false)...).Build()
	s := &Service{client: c, fetchReleaseIndex: testReleaseIndex, sendMail: func(context.Context, mailer.Settings, mailer.Message) error { return errors.New("must not send") }}
	h := &HTTPServer{Service: s}
	rec := httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(`{"event":"release-pending","release":"v0.6.0","from":"v0.5.1"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured mail = %d %s", rec.Code, rec.Body.String())
	}
	// Configured, but nobody with an address.
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(notifyFixtures(true)...).Build()
	s = &Service{client: c, fetchReleaseIndex: testReleaseIndex,
		sendMail: func(context.Context, mailer.Settings, mailer.Message) error { return errors.New("must not send") },
		listGroupMembers: func(context.Context, string, string) ([]authz.KeycloakUser, error) {
			return []authz.KeycloakUser{{Username: "nomail", Enabled: true}}, nil
		},
	}
	h = &HTTPServer{Service: s}
	rec = httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(`{"event":"release-pending","release":"v0.6.0","from":"v0.5.1"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "mail address") {
		t.Fatalf("no address = %d %s", rec.Code, rec.Body.String())
	}
	// No release index (an air-gapped cluster): no release mail.
	s = &Service{client: c, sendMail: func(context.Context, mailer.Settings, mailer.Message) error { return errors.New("must not send") }}
	h = &HTTPServer{Service: s}
	rec = httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(`{"event":"release-pending","release":"v0.6.0","from":"v0.5.1"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no index = %d %s", rec.Code, rec.Body.String())
	}
}

// The director names an event and releases; nothing it sends is written
// into a mail. What no event or no release matches is refused, and the
// mail carries the index's values, never the request's.
func TestAMailIsWrittenFromTheOperatorsOwnValues(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(notifyFixtures(true)...).Build()
	var sent []mailer.Message
	s := &Service{client: c, fetchReleaseIndex: testReleaseIndex,
		sendMail: func(_ context.Context, _ mailer.Settings, m mailer.Message) error {
			sent = append(sent, m)
			return nil
		},
		listGroupMembers: func(context.Context, string, string) ([]authz.KeycloakUser, error) {
			return []authz.KeycloakUser{{Username: "ops@example.org", Enabled: true}}, nil
		},
	}
	h := &HTTPServer{Service: s}
	for name, body := range map[string]string{
		"free text":        `{"subject":"Bcc: x@evil.example","text":"hello"}`,
		"unknown event":    `{"event":"anything","release":"v0.6.0"}`,
		"unlisted release": `{"event":"release-pending","release":"v0.6.0\r\nBcc: x@evil.example","from":"v0.5.1"}`,
		"unknown mode":     `{"event":"release-moved","release":"v0.5.1","from":"v0.5.0","mode":"patch\nBcc: x@evil.example"}`,
	} {
		rec := httptest.NewRecorder()
		h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(sent) != 0 {
		t.Fatalf("sent on a refused request: %+v", sent)
	}
	// A move, the "from" release unlisted: said without it.
	rec := httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath,
		strings.NewReader(`{"event":"release-moved","release":"v0.5.1","from":"v0.4.9-whatever","mode":"patch"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("move = %d %s", rec.Code, rec.Body.String())
	}
	if len(sent) != 1 || sent[0].Subject != "Gentian OS: cluster example.org moves to v0.5.1" ||
		!strings.Contains(sent[0].Text, "was on an earlier release and moves to v0.5.1 now, by its autoUpgrade mode patch") ||
		strings.Contains(sent[0].Text, "whatever") {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestEveryEventWritesASubjectAndAText(t *testing.T) {
	for event, write := range adminMails {
		subject, text := write(mailFacts{Cluster: "example.org", Release: releaseindex.Release{Version: "v0.6.0"}, Mode: releaseindex.ModePatch})
		if subject == "" || text == "" || strings.ContainsAny(subject, "\r\n") {
			t.Errorf("%s: subject %q, text %q", event, subject, text)
		}
	}
}

func TestAnEmptyRequestIsRefused(t *testing.T) {
	h := &HTTPServer{Service: &Service{}}
	rec := httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty = %d", rec.Code)
	}
}
