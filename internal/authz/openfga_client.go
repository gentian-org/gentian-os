// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultHTTPTimeout = 15 * time.Second

// TupleKey is an OpenFGA relationship tuple key.
type TupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

// Tuple is a relationship tuple for write operations.
type Tuple struct {
	User          string `json:"user"`
	Relation      string `json:"relation"`
	Object        string `json:"object"`
	Condition     any    `json:"condition,omitempty"`
	ConditionName string `json:"condition_name,omitempty"`
}

// OpenFGAClient talks to the OpenFGA HTTP API.
type OpenFGAClient struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

func NewOpenFGAClient(baseURL, apiToken string) *OpenFGAClient {
	return &OpenFGAClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

func (c *OpenFGAClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("openfga health: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

type storeListResponse struct {
	Stores []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"stores"`
}

func (c *OpenFGAClient) FindStoreByName(ctx context.Context, name string) (string, bool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/stores", nil)
	if err != nil {
		return "", false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, c.readAPIError(resp)
	}
	var out storeListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, err
	}
	for _, s := range out.Stores {
		if s.Name == name {
			return s.ID, true, nil
		}
	}
	return "", false, nil
}

func (c *OpenFGAClient) CreateStore(ctx context.Context, name string) (string, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/stores", body)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", c.readAPIError(resp)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("openfga create store: empty id")
	}
	return out.ID, nil
}

func (c *OpenFGAClient) WriteAuthorizationModel(ctx context.Context, storeID string) (string, error) {
	schemaVersion, typeDefs, err := AuthorizationModelPayload()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"schema_version":   schemaVersion,
		"type_definitions": typeDefs,
	})
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("/stores/%s/authorization-models", storeID)
	req, err := c.newRequest(ctx, http.MethodPost, path, payload)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", c.readAPIError(resp)
	}
	var out struct {
		AuthorizationModelID string `json:"authorization_model_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.AuthorizationModelID, nil
}

func (c *OpenFGAClient) WriteTuples(ctx context.Context, storeID string, tuples []Tuple, deletes []TupleKey) error {
	writes := make([]TupleKey, 0, len(tuples))
	for _, t := range tuples {
		writes = append(writes, TupleKey{User: t.User, Relation: t.Relation, Object: t.Object})
	}
	if deletes == nil {
		deletes = []TupleKey{}
	}
	payload, err := json.Marshal(map[string]any{
		"writes":  writes,
		"deletes": deletes,
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/stores/%s/write", storeID)
	req, err := c.newRequest(ctx, http.MethodPost, path, payload)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.readAPIError(resp)
	}
	return nil
}

func (c *OpenFGAClient) Check(ctx context.Context, storeID, user, relation, object string) (bool, error) {
	payload, err := json.Marshal(map[string]any{
		"tuple_key": TupleKey{
			User:     user,
			Relation: relation,
			Object:   object,
		},
	})
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("/stores/%s/check", storeID)
	req, err := c.newRequest(ctx, http.MethodPost, path, payload)
	if err != nil {
		return false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, c.readAPIError(resp)
	}
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Allowed, nil
}

func (c *OpenFGAClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	return req, nil
}

func (c *OpenFGAClient) readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("openfga api %s: %s", resp.Status, msg)
}
