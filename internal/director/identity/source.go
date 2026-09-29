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

package identity

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DirectorySource reads one credential per realm from a mounted Secret.
//
// A file per realm, named for the realm, holding that realm's client secret.
// The client id is the same in every realm, so it is configured once rather
// than repeated in every file.
//
// RE-READ, not read once. The operator writes a realm's key when the realm
// appears, and for every tenant that is after this process started; a source
// that read the directory at boot would never speak for a tenant created
// since, and the symptom would be a screen that works for the kernel realm
// and 503s for everything newer until somebody restarted the pod.
//
// The kubelet updates a projected Secret by swapping a symlinked directory, so
// a re-read sees the whole new set at once and never a half-written one.
type DirectorySource struct {
	dir      string
	clientID string
	// ttl bounds how often the directory is walked. The calls that use this
	// are administrative rather than hot, and a few seconds of staleness on a
	// credential that changes when a tenant is created is not worth a watch.
	ttl time.Duration

	mu      sync.Mutex
	cache   map[string]Credential
	refresh time.Time
}

// NewDirectorySource reads realm credentials from dir. A directory that does
// not exist is not an error: a cluster whose operator has not written the
// Secret yet has no realms to speak for, and the routes that need one refuse
// individually with a reason naming the realm.
func NewDirectorySource(dir, clientID string) *DirectorySource {
	return &DirectorySource{dir: dir, clientID: clientID, ttl: 10 * time.Second}
}

func (s *DirectorySource) For(realm string) (Credential, bool) {
	c, ok := s.load()[realm]
	return c, ok
}

func (s *DirectorySource) Realms() []string {
	loaded := s.load()
	out := make([]string, 0, len(loaded))
	for realm := range loaded {
		out = append(out, realm)
	}
	return out
}

func (s *DirectorySource) load() map[string]Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil && time.Now().Before(s.refresh) {
		return s.cache
	}
	next := map[string]Credential{}
	entries, err := os.ReadDir(s.dir)
	if err == nil {
		for _, e := range entries {
			// A projected Secret carries ..data and ..2026_01_01_… entries
			// alongside the keys. They are the mechanism, not credentials.
			name := e.Name()
			if strings.HasPrefix(name, "..") || strings.HasPrefix(name, ".") {
				continue
			}
			if e.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(s.dir, name))
			if err != nil {
				continue
			}
			secret := strings.TrimSpace(string(raw))
			if secret == "" {
				continue
			}
			next[name] = Credential{
				Realm:        name,
				TokenRealm:   name,
				ClientID:     s.clientID,
				ClientSecret: secret,
			}
		}
	}
	// Kept even when the read failed, which yields an empty set rather than
	// the previous one. A credential that was withdrawn must stop working;
	// holding the last good set would keep a retired realm reachable.
	s.cache = next
	s.refresh = time.Now().Add(s.ttl)
	return next
}
