/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The installed adapters, as the tool servers they are. An adapter is an MCP
// server inside the tenant. What a tool is, class included, is read from the
// adapter's own list: the adapter was reviewed as a whole, and its list is
// all the chaperone has to go by.

const (
	toolsTTL = time.Minute
	// ClassKey is where an adapter's list says a tool's class, under _meta.
	ClassKey = "gentianos.io/class"
)

var toolName = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*)\.v([1-9][0-9]*)$`)

// Installed is one adapter: its name, the address of its MCP endpoint and
// the profile of the app it adapts.
type Installed struct {
	Name string
	URL  string
	App  string
}

// ParseInstalled reads the adapters a setting names: a JSON object from an
// adapter's name to its address and its app,
// {"odoo": {"url": "http://odoo-adapter:8765/mcp", "app": "odoo-base-ce"}}.
// An entry without an address or an app is an error of the install.
func ParseInstalled(setting string) ([]Installed, error) {
	if strings.TrimSpace(setting) == "" {
		return nil, nil
	}
	var raw map[string]struct {
		URL string `json:"url"`
		App string `json:"app"`
	}
	if err := json.Unmarshal([]byte(setting), &raw); err != nil {
		return nil, fmt.Errorf("the adapters are a JSON object from an adapter's name to {url, app}: %w", err)
	}
	found := make([]Installed, 0, len(raw))
	for name, entry := range raw {
		if entry.URL == "" || entry.App == "" {
			return nil, fmt.Errorf("the adapter %s needs a url and an app", name)
		}
		found = append(found, Installed{Name: name, URL: entry.URL, App: entry.App})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found, nil
}

// McpAdapters is both what the tools are (Tools) and how a call reaches one
// (Adapters).
type McpAdapters struct {
	installed map[string]Installed
	client    *http.Client

	mu      sync.Mutex
	tools   map[string]Tool
	fetched time.Time
}

// NewMcpAdapters serves the adapters given.
func NewMcpAdapters(installed []Installed, client *http.Client) *McpAdapters {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	byName := map[string]Installed{}
	for _, a := range installed {
		byName[a.Name] = a
	}
	return &McpAdapters{installed: byName, client: client}
}

type rpcAnswer struct {
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Result json.RawMessage `json:"result"`
}

// answer is the one JSON-RPC answer of a request, sent plainly or as a
// stream.
func answer(resp *http.Response) (rpcAnswer, error) {
	var out rpcAnswer
	body := io.LimitReader(resp.Body, 16<<20)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return out, json.NewDecoder(body).Decode(&out)
	}
	last := ""
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data:"); ok {
			last = strings.TrimSpace(data)
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	if last == "" {
		return out, errors.New("an empty stream")
	}
	return out, json.Unmarshal([]byte(last), &out)
}

func (m *McpAdapters) rpc(ctx context.Context, adapter Installed, method string, params any, credential string, result any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return adapterError("%s could not be asked: %v", adapter.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.URL, bytes.NewReader(body))
	if err != nil {
		return adapterError("%s could not be asked: %v", adapter.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return adapterError("%s could not be asked: %v", adapter.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return adapterError("%s answered %d", adapter.Name, resp.StatusCode)
	}
	got, err := answer(resp)
	if err != nil {
		return adapterError("%s could not be asked: %v", adapter.Name, err)
	}
	if got.Error != nil {
		if got.Error.Message == "" {
			return adapterError("the adapter failed")
		}
		return adapterError("%s", got.Error.Message)
	}
	if len(got.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(got.Result, result); err != nil {
		return adapterError("%s could not be asked: %v", adapter.Name, err)
	}
	return nil
}

func (m *McpAdapters) list(ctx context.Context, adapter Installed) ([]Tool, error) {
	var listed struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Meta        map[string]any `json:"_meta"`
		} `json:"tools"`
	}
	if err := m.rpc(ctx, adapter, "tools/list", map[string]any{}, "", &listed); err != nil {
		return nil, err
	}
	var found []Tool
	for _, raw := range listed.Tools {
		match := toolName.FindStringSubmatch(raw.Name)
		if match == nil {
			continue
		}
		version, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}
		class := ClassIrreversible
		if said, _ := raw.Meta[ClassKey].(string); said == ClassRead || said == ClassChange {
			class = said
		}
		schema := raw.InputSchema
		if schema == nil {
			schema = map[string]any{}
		}
		found = append(found, Tool{
			Adapter: adapter.Name, Name: match[1], Version: version, App: adapter.App,
			Class: class, Description: raw.Description, InputSchema: schema,
		})
	}
	return found, nil
}

func (m *McpAdapters) load(ctx context.Context) map[string]Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tools) > 0 && time.Since(m.fetched) <= toolsTTL {
		return m.tools
	}
	tools := map[string]Tool{}
	for _, adapter := range m.installed {
		listed, err := m.list(ctx, adapter)
		if err != nil {
			// An adapter that does not answer offers nothing. The others
			// are not held up by it.
			continue
		}
		for _, t := range listed {
			tools[t.Key()] = t
		}
	}
	m.tools, m.fetched = tools, time.Now()
	return tools
}

func (m *McpAdapters) Lookup(ctx context.Context, key string) (Tool, bool) {
	tool, ok := m.load(ctx)[key]
	return tool, ok
}

func (m *McpAdapters) All(ctx context.Context) []Tool {
	tools := m.load(ctx)
	all := make([]Tool, 0, len(tools))
	for _, t := range tools {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key() < all[j].Key() })
	return all
}

func (m *McpAdapters) Call(ctx context.Context, tool Tool, arguments map[string]any, credential string) (any, error) {
	if credential == "" {
		return nil, adapterError("there is no credential to make this call as the person")
	}
	adapter, ok := m.installed[tool.Adapter]
	if !ok {
		return nil, adapterError("%s is not installed", tool.Adapter)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	var result struct {
		IsError    bool            `json:"isError"`
		Content    json.RawMessage `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	if err := m.rpc(ctx, adapter, "tools/call",
		map[string]any{"name": fmt.Sprintf("%s.v%d", tool.Name, tool.Version), "arguments": arguments},
		credential, &result); err != nil {
		return nil, err
	}
	if result.IsError {
		var content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(result.Content, &content)
		var texts []string
		for _, c := range content {
			if c.Type == "text" && c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
		if len(texts) == 0 {
			return nil, adapterError("the tool failed")
		}
		return nil, adapterError("%s", strings.Join(texts, " "))
	}
	if len(result.Structured) > 0 {
		return result.Structured, nil
	}
	if len(result.Content) > 0 {
		return result.Content, nil
	}
	return json.RawMessage("[]"), nil
}
