/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// httpReleaseResolver answers a track's questions over HTTPS, with nothing
// but what an anonymous reader gets: a git repository's advertised refs,
// and a registry's manifests under a pull token. GitHub and ghcr.io are the
// defaults; any host that speaks the smart HTTP protocol and the OCI
// distribution API answers the same.
type httpReleaseResolver struct {
	HTTP *http.Client
}

var refLine = regexp.MustCompile(`([0-9a-f]{40})\s+refs/heads/(\S+)`)

// BranchHead reads the repository's advertised refs
// (<repo>/info/refs?service=git-upload-pack) and returns the branch's.
func (h *httpReleaseResolver) BranchHead(ctx context.Context, repoURL, branch string) (string, error) {
	url := strings.TrimSuffix(strings.TrimSuffix(repoURL, "/"), ".git") + ".git/info/refs?service=git-upload-pack"
	body, status, err := h.get(ctx, url, "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, status)
	}
	for _, m := range refLine.FindAllStringSubmatch(string(body), -1) {
		if strings.TrimRight(m[2], "\x00") == branch {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("%s has no branch %s", repoURL, branch)
}

// ImageExists asks the registry for the tag's manifest.
func (h *httpReleaseResolver) ImageExists(ctx context.Context, image string) (bool, error) {
	host, repo, tag, err := splitImage(image)
	if err != nil {
		return false, err
	}
	token, err := h.pullToken(ctx, host, repo)
	if err != nil {
		return false, err
	}
	_, status, err := h.get(ctx, fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag), token)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("%s: HTTP %d", image, status)
}

// NewestBuild lists the chart's versions (tags/list, every page) and
// returns the highest build of a branch among them.
func (h *httpReleaseResolver) NewestBuild(ctx context.Context, repo string) (string, error) {
	host, name, _ := strings.Cut(repo, "/")
	token, err := h.pullToken(ctx, host, name)
	if err != nil {
		return "", err
	}
	best := ""
	url := fmt.Sprintf("https://%s/v2/%s/tags/list?n=1000", host, name)
	for page := 0; url != ""; page++ {
		if page == maxTagPages {
			return "", fmt.Errorf("chart %s lists more than %d pages of versions", repo, maxTagPages)
		}
		body, status, header, err := h.do(ctx, url, token)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("chart %s: its versions answer HTTP %d", repo, status)
		}
		var list struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return "", fmt.Errorf("chart %s: its versions cannot be read: %w", repo, err)
		}
		for _, tag := range list.Tags {
			if newerBuild(tag, best) {
				best = tag
			}
		}
		url = nextPage(host, header.Get("Link"))
	}
	if best == "" {
		return "", fmt.Errorf("chart %s has no build of a branch (X.Y.Z-dev.<n>.g<commit>)", repo)
	}
	return best, nil
}

// maxTagPages bounds the walk over a chart's versions: a registry that
// keeps naming a next page is not followed forever.
const maxTagPages = 200

var (
	branchBuild = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)-dev\.([0-9]+)\.g[0-9a-f]+$`)
	linkNext    = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)
)

// newerBuild says whether a is a build of a branch and sorts above b, by
// semver: the release it leads up to first, then its number, which grows
// with every commit. An empty or foreign b is below every build.
func newerBuild(a, b string) bool {
	am := branchBuild.FindStringSubmatch(a)
	if am == nil {
		return false
	}
	bm := branchBuild.FindStringSubmatch(b)
	if bm == nil {
		return true
	}
	for i := 1; i <= 4; i++ {
		x, _ := strconv.ParseUint(am[i], 10, 64)
		y, _ := strconv.ParseUint(bm[i], 10, 64)
		if x != y {
			return x > y
		}
	}
	return false
}

// nextPage is the address a Link header names as the next page of a list,
// which a registry gives relative to itself; "" when there is none.
func nextPage(host, link string) string {
	m := linkNext.FindStringSubmatch(link)
	if m == nil {
		return ""
	}
	if strings.HasPrefix(m[1], "/") {
		return "https://" + host + m[1]
	}
	return m[1]
}

func (h *httpReleaseResolver) pullToken(ctx context.Context, host, repo string) (string, error) {
	body, status, err := h.get(ctx, fmt.Sprintf("https://%s/token?scope=repository:%s:pull&service=%s", host, repo, host), "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("no pull token for %s from %s (HTTP %d)", repo, host, status)
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.Token == "" {
		return "", fmt.Errorf("no pull token for %s from %s", repo, host)
	}
	return token.Token, nil
}

func (h *httpReleaseResolver) get(ctx context.Context, url, token string) ([]byte, int, error) {
	body, status, _, err := h.do(ctx, url, token)
	return body, status, err
}

// do is get with the answer's headers.
func (h *httpReleaseResolver) do(ctx context.Context, url, token string) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/json", "*/*",
	}, ", "))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := h.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, res.StatusCode, res.Header, err
	}
	return body, res.StatusCode, res.Header, nil
}

// splitImage reads "host/repo/name:tag".
func splitImage(image string) (host, repo, tag string, err error) {
	ref, tag, ok := strings.Cut(image, ":")
	if !ok || tag == "" {
		return "", "", "", fmt.Errorf("image %q names no tag", image)
	}
	host, repo, ok = strings.Cut(ref, "/")
	if !ok || repo == "" {
		return "", "", "", fmt.Errorf("image %q names no registry", image)
	}
	return host, repo, tag, nil
}
