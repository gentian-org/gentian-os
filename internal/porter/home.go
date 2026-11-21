/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package porter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Refusal is something a person can be told: the status the page is
// answered with, and words that name nothing of the home's or the realm's.
type Refusal struct {
	Status  int
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(status int, message string) error { return &Refusal{Status: status, Message: message} }

// The entries of the Secret vouching-<profile> the porter reads
// (internal/controller/vouching.go).
const (
	toldClientID     = "VOUCHING_CLIENT_ID"
	toldClientSecret = "VOUCHING_CLIENT_SECRET" //nolint:gosec // Secret key name, not a credential.
	toldTokenURL     = "VOUCHING_TOKEN_URL"     //nolint:gosec // Secret key name, not a credential.
	toldHomeScope    = "VOUCHING_HOME_FILE_STORE"
)

// Entry is one file or folder of a listing.
type Entry struct {
	Name     string `json:"name"`
	Folder   bool   `json:"folder"`
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
	Type     string `json:"type,omitempty"`
}

// Picked is what an app's page is told of a file a person picked.
type Picked struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	URL  string `json:"url"`
}

// Homes reaches the tenant's file home as the person a request is for.
type Homes struct {
	// VouchingDir is the Secret vouching-<profile>, mounted. Read each time:
	// the platform changes it while the service runs.
	VouchingDir string
	Client      *http.Client

	mu       sync.Mutex
	sessions map[[32]byte]*session
}

// session is what one token of the edge's is worth at the home, for as long
// as the home's token lasts.
type session struct {
	token   string
	root    string // the person's files, ending in a slash
	expires time.Time
}

// NewHomes is a Homes whose calls follow nobody's redirect: a person's token
// goes to the realm and the home, and nowhere they point.
func NewHomes(vouchingDir string) *Homes {
	return &Homes{
		VouchingDir: vouchingDir,
		Client: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sessions: map[[32]byte]*session{},
	}
}

func (h *Homes) told(key string) string {
	b, err := os.ReadFile(filepath.Join(h.VouchingDir, key)) //nolint:gosec // The directory is the service's own setting.
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// sessionFor exchanges the token the edge handed a request for one valid at
// the home, and finds the person's files there. Kept by the edge's token and
// until the home's token ends: a request without that token gets nothing.
func (h *Homes) sessionFor(ctx context.Context, home *Home, edgeToken string) (*session, error) {
	if home == nil {
		return nil, refuse(http.StatusServiceUnavailable, "This workspace has no place for files.")
	}
	if edgeToken == "" {
		return nil, refuse(http.StatusUnauthorized, "You are not signed in.")
	}
	key := sha256.Sum256([]byte(home.Profile + "\n" + edgeToken))
	now := time.Now()
	h.mu.Lock()
	for k, s := range h.sessions {
		if now.After(s.expires) {
			delete(h.sessions, k)
		}
	}
	s := h.sessions[key]
	h.mu.Unlock()
	if s != nil {
		return s, nil
	}
	token, lifetime, err := h.exchange(ctx, edgeToken)
	if err != nil {
		return nil, err
	}
	root, err := h.findRoot(ctx, home, token)
	if err != nil {
		return nil, err
	}
	// Ended a little early, so a call in flight does not arrive expired.
	s = &session{token: token, root: root, expires: now.Add(lifetime - 15*time.Second)}
	if lifetime > 30*time.Second {
		h.mu.Lock()
		h.sessions[key] = s
		h.mu.Unlock()
	}
	return s, nil
}

// exchange is the realm's standard token exchange (RFC 8693): the person's
// token for the porter, for one of the home.
func (h *Homes) exchange(ctx context.Context, edgeToken string) (string, time.Duration, error) {
	scope, tokenURL := h.told(toldHomeScope), h.told(toldTokenURL)
	if scope == "" || tokenURL == "" {
		return "", 0, refuse(http.StatusServiceUnavailable, "This workspace has no place for files.")
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"client_id":          {h.told(toldClientID)},
		"client_secret":      {h.told(toldClientSecret)},
		"subject_token":      {edgeToken},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":              {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.Client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var refusal struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &refusal)
		// The realm's words go to the log, not to the page.
		detail := fmt.Errorf("token exchange: %d %s: %s", resp.StatusCode, refusal.Error, refusal.Description)
		if resp.StatusCode >= 500 {
			return "", 0, detail
		}
		return "", 0, fmt.Errorf("%w: %w", refuse(http.StatusForbidden, "The workspace gave no access to your files."), detail)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.AccessToken == "" {
		return "", 0, errors.New("token exchange: an answer with no token")
	}
	return answer.AccessToken, time.Duration(answer.ExpiresIn) * time.Second, nil
}

// multistatus is the answer of a PROPFIND, as far as it is read here. Every
// property is kept by its name, so the home's id property is found whatever
// it is called.
type multistatus struct {
	Responses []struct {
		Href      string `xml:"href"`
		Propstats []struct {
			Status string `xml:"status"`
			Prop   struct {
				Any []property `xml:",any"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

type property struct {
	XMLName xml.Name
	Text    string `xml:",chardata"`
	Inner   []byte `xml:",innerxml"`
}

const nsDAV = "DAV:"

// props are the properties of one response that the home answered with 200.
func (m *multistatus) props(i int) map[xml.Name]property {
	out := map[xml.Name]property{}
	for _, ps := range m.Responses[i].Propstats {
		if !strings.Contains(ps.Status, " 200") {
			continue
		}
		for _, p := range ps.Prop.Any {
			out[p.XMLName] = p
		}
	}
	return out
}

func (h *Homes) propfind(ctx context.Context, token, target, depth, props string) (*multistatus, int, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop>` + props + `</d:prop></d:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", target, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("the home could not be asked: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, resp.StatusCode, nil
	}
	ms := &multistatus{}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(ms); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("the home's answer is not a multistatus: %w", err)
	}
	return ms, resp.StatusCode, nil
}

// findRoot is where the person's files are at the home: the root its
// profile declares, with the person's principal put in where it asks for
// one.
func (h *Homes) findRoot(ctx context.Context, home *Home, token string) (string, error) {
	base := strings.TrimSuffix(home.URL, "/") + "/"
	root := strings.TrimPrefix(home.Root, "/")
	if strings.Contains(root, "{principal}") {
		ms, status, err := h.propfind(ctx, token, base, "0", `<d:current-user-principal/>`)
		if err != nil {
			return "", err
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return "", refuse(http.StatusForbidden, "The place for files did not accept your sign-in.")
		}
		name := ""
		if ms != nil {
			for i := range ms.Responses {
				if p, ok := ms.props(i)[xml.Name{Space: nsDAV, Local: "current-user-principal"}]; ok {
					name = path.Base(strings.TrimSuffix(decoded(innerText(p.Inner)), "/"))
				}
			}
		}
		if name == "" || name == "." || name == "/" {
			return "", fmt.Errorf("the home at %s names no principal (status %d)", base, status)
		}
		root = strings.ReplaceAll(root, "{principal}", url.PathEscape(name))
	}
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return base + root, nil
}

// innerText is the first text inside a property's elements: the href of a
// principal, whatever prefix the home writes its elements with.
func innerText(inner []byte) string {
	d := xml.NewDecoder(bytes.NewReader(inner))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		if c, ok := tok.(xml.CharData); ok {
			if t := strings.TrimSpace(string(c)); t != "" {
				return t
			}
		}
	}
}

// decoded is a path the home named, unescaped; as it is where it cannot be.
func decoded(href string) string {
	if u, err := url.Parse(href); err == nil {
		return u.Path
	}
	return href
}

// target is the address of a path below the person's files. The path is the
// page's and is trusted for nothing: it is cleaned, and may not leave the
// root.
func (s *session) target(p string) (string, error) {
	clean := path.Clean("/" + p)
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", refuse(http.StatusBadRequest, "That is no place in your files.")
		}
	}
	root, err := url.Parse(s.root)
	if err != nil {
		return "", err
	}
	// Only the path is the page's: the scheme and the host are the root's,
	// copied from it, and the result is held to the root once more.
	u := &url.URL{Scheme: root.Scheme, Host: root.Host, Path: root.Path}
	if rel := strings.Trim(clean, "/"); rel != "" {
		u = u.JoinPath(strings.Split(rel, "/")...)
		if strings.HasSuffix(root.Path, "/") && strings.HasSuffix(clean, "/") {
			u.Path += "/"
		}
	}
	if u.Host != root.Host || u.Scheme != root.Scheme || !strings.HasPrefix(u.Path, root.Path) {
		return "", refuse(http.StatusBadRequest, "That is no place in your files.")
	}
	return u.String(), nil
}

func idProps(home *Home) (xml.Name, string) {
	space, name, _ := splitProperty(home.IDProperty)
	return xml.Name{Space: space, Local: name}, `<x:` + name + ` xmlns:x="` + xmlEscape(space) + `"/>`
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

const listProps = `<d:resourcetype/><d:getcontentlength/><d:getlastmodified/><d:getcontenttype/>`

// List is the folder at the path of the person's files: folders first, then
// files, each by name.
func (h *Homes) List(ctx context.Context, home *Home, edgeToken, p string) ([]Entry, error) {
	s, err := h.sessionFor(ctx, home, edgeToken)
	if err != nil {
		return nil, err
	}
	target, err := s.target(p)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(target, "/") {
		target += "/"
	}
	ms, status, err := h.propfind(ctx, s.token, target, "1", listProps)
	if err != nil {
		return nil, err
	}
	if ms == nil {
		return nil, homeRefusal(status)
	}
	self := strings.TrimSuffix(decoded(target), "/")
	out := []Entry{}
	for i, r := range ms.Responses {
		href := strings.TrimSuffix(decoded(r.Href), "/")
		if href == self || href == "" {
			continue
		}
		out = append(out, entryOf(path.Base(href), ms.props(i)))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Folder != out[j].Folder {
			return out[i].Folder
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func entryOf(name string, props map[xml.Name]property) Entry {
	e := Entry{Name: name}
	if rt, ok := props[xml.Name{Space: nsDAV, Local: "resourcetype"}]; ok {
		e.Folder = bytes.Contains(rt.Inner, []byte("collection"))
	}
	if !e.Folder {
		e.Size, _ = strconv.ParseInt(strings.TrimSpace(props[xml.Name{Space: nsDAV, Local: "getcontentlength"}].Text), 10, 64)
		e.Type = strings.TrimSpace(props[xml.Name{Space: nsDAV, Local: "getcontenttype"}].Text)
	}
	if t, err := http.ParseTime(strings.TrimSpace(props[xml.Name{Space: nsDAV, Local: "getlastmodified"}].Text)); err == nil {
		e.Modified = t.UTC().Format(time.RFC3339)
	}
	return e
}

// Pick is the file at the path, as an app's page is told it: its name, its
// id at the home and the address that opens it by that id.
func (h *Homes) Pick(ctx context.Context, home *Home, edgeToken, p string) (*Picked, error) {
	s, err := h.sessionFor(ctx, home, edgeToken)
	if err != nil {
		return nil, err
	}
	target, err := s.target(p)
	if err != nil {
		return nil, err
	}
	idName, idProp := idProps(home)
	ms, status, err := h.propfind(ctx, s.token, target, "0", `<d:resourcetype/>`+idProp)
	if err != nil {
		return nil, err
	}
	if ms == nil || len(ms.Responses) == 0 {
		return nil, homeRefusal(status)
	}
	props := ms.props(0)
	if rt, ok := props[xml.Name{Space: nsDAV, Local: "resourcetype"}]; ok && bytes.Contains(rt.Inner, []byte("collection")) {
		return nil, refuse(http.StatusBadRequest, "That is a folder, not a file.")
	}
	id := strings.TrimSpace(props[idName].Text)
	if id == "" {
		return nil, fmt.Errorf("the home names no %s for %s", home.IDProperty, target)
	}
	return &Picked{
		Name: path.Base(strings.TrimSuffix(decoded(target), "/")),
		ID:   id,
		URL:  strings.ReplaceAll(home.Open, "{id}", url.PathEscape(id)),
	}, nil
}

// homeRefusal is what a person is told of an answer of the home's that is
// no listing.
func homeRefusal(status int) error {
	switch status {
	case http.StatusNotFound:
		return refuse(http.StatusNotFound, "That is not in your files any more.")
	case http.StatusUnauthorized, http.StatusForbidden:
		return refuse(http.StatusForbidden, "The place for files did not let you see that.")
	}
	return fmt.Errorf("the home answered %d", status)
}
