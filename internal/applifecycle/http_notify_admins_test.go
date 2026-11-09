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
)

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
	s := &Service{client: c,
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
	body := `{"subject":"Gentian OS v0.6.0 waits for you","text":"v0.6.0 is out.\nkubectl gentian release set v0.6.0\n"}`
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
	if len(sent) != 1 || sent[0].Subject != "Gentian OS v0.6.0 waits for you" || !strings.Contains(sent[0].Text, "release set v0.6.0") {
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
	s := &Service{client: c, sendMail: func(context.Context, mailer.Settings, mailer.Message) error { return errors.New("must not send") }}
	h := &HTTPServer{Service: s}
	rec := httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(`{"subject":"s","text":"t"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured mail = %d %s", rec.Code, rec.Body.String())
	}
	// Configured, but nobody with an address.
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(notifyFixtures(true)...).Build()
	s = &Service{client: c,
		sendMail: func(context.Context, mailer.Settings, mailer.Message) error { return errors.New("must not send") },
		listGroupMembers: func(context.Context, string, string) ([]authz.KeycloakUser, error) {
			return []authz.KeycloakUser{{Username: "nomail", Enabled: true}}, nil
		},
	}
	h = &HTTPServer{Service: s}
	rec = httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(`{"subject":"s","text":"t"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "mail address") {
		t.Fatalf("no address = %d %s", rec.Code, rec.Body.String())
	}
	// An empty body is the caller's mistake.
	rec = httptest.NewRecorder()
	h.handleNotifyAdministrators(rec, httptest.NewRequest(http.MethodPost, NotifyAdministratorsPath, strings.NewReader(`{"subject":"","text":""}`)))
	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusBadRequest {
		t.Fatalf("empty = %d", rec.Code)
	}
}
