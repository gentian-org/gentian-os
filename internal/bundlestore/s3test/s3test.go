/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package s3test is an object store in memory, for tests: as much of S3 as
// minio-go asks of it to make a bucket, put, read, list and remove objects,
// path-style and unsigned-checked. Nothing outside tests imports it.
package s3test

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is the store. Objects are keyed "bucket/key".
type Server struct {
	URL string

	mu      sync.Mutex
	buckets map[string]bool
	objects map[string]object
}

type object struct {
	data     []byte
	modified time.Time
}

// New starts a store, closed when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{buckets: map[string]bool{}, objects: map[string]object{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Put stores an object, made a bucket as needed, as if written at modified.
func (s *Server) Put(bucket, key string, data []byte, modified time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[bucket] = true
	s.objects[bucket+"/"+key] = object{data: data, modified: modified}
}

// Keys are the keys in a bucket, sorted.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if rest, ok := strings.CutPrefix(k, bucket+"/"); ok {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}

// Get is an object's content, nil when there is none.
func (s *Server) Get(bucket, key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[bucket+"/"+key].data
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case key == "" && q.Has("location"):
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
	case key == "" && r.Method == http.MethodPut:
		if s.buckets[bucket] {
			s.fail(w, http.StatusConflict, "BucketAlreadyOwnedByYou")
			return
		}
		s.buckets[bucket] = true
	case key == "" && r.Method == http.MethodHead:
		if !s.buckets[bucket] {
			w.WriteHeader(http.StatusNotFound)
		}
	case key == "" && r.Method == http.MethodGet:
		s.list(w, bucket, q.Get("prefix"), q.Get("delimiter"))
	case r.Method == http.MethodPut:
		if !s.buckets[bucket] {
			s.fail(w, http.StatusNotFound, "NoSuchBucket")
			return
		}
		data, err := readBody(r)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		s.objects[bucket+"/"+key] = object{data: data, modified: time.Now().UTC()}
		w.Header().Set("ETag", `"0"`)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		obj, ok := s.objects[bucket+"/"+key]
		if !ok {
			s.fail(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
		w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"0"`)
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj.data)
		}
	case r.Method == http.MethodDelete:
		delete(s.objects, bucket+"/"+key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s.fail(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

type listResult struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	KeyCount       int            `xml:"KeyCount"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []listContent  `xml:"Contents"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

type listContent struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	Size         int64  `xml:"Size"`
	ETag         string `xml:"ETag"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

func (s *Server) list(w http.ResponseWriter, bucket, prefix, delimiter string) {
	if !s.buckets[bucket] {
		s.fail(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	out := listResult{Name: bucket, Prefix: prefix}
	seen := map[string]bool{}
	var keys []string
	for k := range s.objects {
		if rest, ok := strings.CutPrefix(k, bucket+"/"); ok && strings.HasPrefix(rest, prefix) {
			keys = append(keys, rest)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				p := k[:len(prefix)+i+len(delimiter)]
				if !seen[p] {
					seen[p] = true
					out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: p})
				}
				continue
			}
		}
		obj := s.objects[bucket+"/"+k]
		out.Contents = append(out.Contents, listContent{Key: k, Size: int64(len(obj.data)),
			LastModified: obj.modified.UTC().Format("2006-01-02T15:04:05.000Z"), ETag: `"0"`})
	}
	out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}

// readBody reads a PUT's body, undoing aws-chunked encoding when the client
// streamed its signature.
func readBody(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") &&
		!strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeHex, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, err
		}
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}
