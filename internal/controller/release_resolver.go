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

// ChartBuild reads the moving chart's config blob, whose appVersion names
// the immutable build the moving version points at (how gentian-ui
// publishes: 0.1.0-develop's appVersion is 0.1.0-develop.<sha>).
func (h *httpReleaseResolver) ChartBuild(ctx context.Context, repo, moving string) (string, error) {
	host, name, _ := strings.Cut(repo, "/")
	token, err := h.pullToken(ctx, host, name)
	if err != nil {
		return "", err
	}
	body, status, err := h.get(ctx, fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, name, moving), token)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("chart %s:%s is not published (HTTP %d)", repo, moving, status)
	}
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.Config.Digest == "" {
		return "", fmt.Errorf("chart %s:%s has no config blob", repo, moving)
	}
	body, status, err = h.get(ctx, fmt.Sprintf("https://%s/v2/%s/blobs/%s", host, name, manifest.Config.Digest), token)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("chart %s:%s: its config blob answers HTTP %d", repo, moving, status)
	}
	var config struct {
		AppVersion string `json:"appVersion"`
	}
	if err := json.Unmarshal(body, &config); err != nil || !strings.HasPrefix(config.AppVersion, moving+".") {
		return "", fmt.Errorf("chart %s:%s names no immutable build (appVersion %q)", repo, moving, config.AppVersion)
	}
	return config.AppVersion, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
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
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	return body, res.StatusCode, nil
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
