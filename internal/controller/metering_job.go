/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

type MeteringWorker struct {
	Reconciler *TenantReconciler
	Interval   time.Duration
}

func (w *MeteringWorker) Start(ctx context.Context) error {
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

func (w *MeteringWorker) RunOnce(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("metering-worker")
	logger.Info("starting metering run")

	var tenants gentianov1alpha1.TenantList
	if err := w.Reconciler.List(ctx, &tenants); err != nil {
		logger.Error(err, "failed to list tenants")
		return
	}

	for _, tenant := range tenants.Items {
		if !tenant.DeletionTimestamp.IsZero() {
			continue
		}

		tenantDomain := tenant.EffectiveDomain(w.Reconciler.KernelDomain, w.Reconciler.TenancyMode)
		if tenantDomain == "" {
			continue
		}

		nsName := tenantNamespaceName(&tenant)
		for _, app := range tenant.Spec.Apps {
			if app.Profile == "" {
				continue
			}

			secretName := "metering-secret-" + app.Profile
			sec := &corev1.Secret{}
			err := w.Reconciler.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, sec)
			if errors.IsNotFound(err) {
				continue
			}
			if err != nil {
				logger.Error(err, "failed to get metering secret", "tenant", tenant.Name, "app", app.Profile)
				continue
			}

			meteringSecret := string(sec.Data["metering-secret"])
			productSku := string(sec.Data["product-sku"])

			// Fetch active user count from Keycloak
			count, err := w.getKeycloakGroupUserCount(ctx, tenant.Name, app.Profile)
			if err != nil {
				logger.Error(err, "failed to fetch keycloak group count", "tenant", tenant.Name, "app", app.Profile)
				continue
			}

			// Submit the report
			if err := w.submitReport(ctx, tenantDomain, productSku, count, meteringSecret); err != nil {
				logger.Error(err, "failed to submit metering report", "tenant", tenant.Name, "app", app.Profile)
			} else {
				logger.Info("metering report submitted successfully", "tenant", tenant.Name, "app", app.Profile, "count", count)
			}
		}
	}
}

func (w *MeteringWorker) getKeycloakGroupUserCount(ctx context.Context, tenantName, profile string) (int, error) {
	kcSec := &corev1.Secret{}
	if err := w.Reconciler.Get(ctx, types.NamespacedName{Name: "keycloak-admin", Namespace: "platform-kernel"}, kcSec); err != nil {
		return 0, fmt.Errorf("failed to read keycloak-admin secret: %w", err)
	}

	kcURL := string(kcSec.Data["url"])
	kcUser := string(kcSec.Data["username"])
	kcPass := string(kcSec.Data["password"])

	// 1. Get access token
	token, err := getKeycloakToken(ctx, kcURL, kcUser, kcPass)
	if err != nil {
		return 0, fmt.Errorf("failed to get Keycloak token: %w", err)
	}

	// 2. Find group ID
	groupName := fmt.Sprintf("gentian:tenant:%s:app:%s", tenantName, profile)
	groupID, err := getKeycloakGroupID(ctx, kcURL, tenantName, token, groupName)
	if err != nil {
		return 0, fmt.Errorf("failed to get Keycloak group ID: %w", err)
	}
	if groupID == "" {
		// Group doesn't exist yet, meaning no users
		return 0, nil
	}

	// 3. Get group members count
	count, err := getKeycloakGroupMembersCount(ctx, kcURL, tenantName, token, groupID)
	if err != nil {
		return 0, fmt.Errorf("failed to get group members count: %w", err)
	}

	return count, nil
}

func getKeycloakToken(ctx context.Context, kcURL, username, password string) (string, error) {
	u := fmt.Sprintf("%s/realms/master/protocol/openid-connect/token", strings.TrimRight(kcURL, "/"))
	data := url.Values{}
	data.Set("client_id", "admin-cli")
	data.Set("grant_type", "password")
	data.Set("username", username)
	data.Set("password", password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed token request (status %d): %s", resp.StatusCode, string(body))
	}

	var res map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	tok, _ := res["access_token"].(string)
	if tok == "" {
		return "", fmt.Errorf("empty access_token in response")
	}
	return tok, nil
}

func getKeycloakGroupID(ctx context.Context, kcURL, realmName, token, groupName string) (string, error) {
	u := fmt.Sprintf("%s/admin/realms/%s/groups?search=%s&exact=true", strings.TrimRight(kcURL, "/"), realmName, url.QueryEscape(groupName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("group search status %d: %s", resp.StatusCode, string(body))
	}

	var groups []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return "", err
	}

	for _, g := range groups {
		if g["name"] == groupName {
			id, _ := g["id"].(string)
			return id, nil
		}
	}
	return "", nil
}

func getKeycloakGroupMembersCount(ctx context.Context, kcURL, realmName, token, groupID string) (int, error) {
	u := fmt.Sprintf("%s/admin/realms/%s/groups/%s/members", strings.TrimRight(kcURL, "/"), realmName, groupID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("group members status %d: %s", resp.StatusCode, string(body))
	}

	var users []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return 0, err
	}
	return len(users), nil
}

type meteringReportRequest struct {
	TenantDomain    string `json:"tenantDomain"`
	ProductSku      string `json:"productSku"`
	Month           string `json:"month"`
	ActiveUserCount int    `json:"activeUserCount"`
	Method          string `json:"method"`
	EvidenceDigest  string `json:"evidenceDigest,omitempty"`
	ReportSignature string `json:"reportSignature"`
}

func (w *MeteringWorker) submitReport(ctx context.Context, tenantDomain, productSku string, count int, secret string) error {
	month := time.Now().Format("2006-01")
	method := "keycloak-group-membership-v1"

	// Signature calculation
	msg := fmt.Sprintf("%s:%s:%s:%d:%s:%s", tenantDomain, productSku, month, count, method, "")
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(msg))
	sig := hex.EncodeToString(h.Sum(nil))

	payload := meteringReportRequest{
		TenantDomain:    tenantDomain,
		ProductSku:      productSku,
		Month:           month,
		ActiveUserCount: count,
		Method:          method,
		ReportSignature: sig,
	}

	url := fmt.Sprintf("%s/api/v1/metering/report", strings.TrimRight(w.Reconciler.CorpAPIURL, "/"))
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.Reconciler.OperatorToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("submit report failed (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}
