/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// A catalogue kept in the deployments repository.
//
// A catalogue is an https address anybody could have fetched from, and no
// credential is ever sent to one. Profiles that must not be public therefore
// have nowhere to be served from -- except the one private place the director
// already reads: the cluster's own deployments repository. A source may name a
// directory of it instead of an address, in the layout an address serves:
// index.yaml, and profiles/<name>.yaml.
//
// What is declared is a directory and nothing else. There is no field for a
// repository, a host, a branch or a revision, so a declaration cannot name
// another repository or another commit: the files are read from the checkout
// this process already holds, at the commit it is at, fetched with the
// credential it already has. No new credential, and no request to anywhere.
//
// The directory is a relative path of plain names. It is checked as it is
// written (CheckCatalogueDirectory), when it is declared and every time it is
// read; and every part of the way to a file is looked at on disk before the
// file is opened, so that a symbolic link committed to the repository is
// refused rather than followed -- to another directory of the repository or
// out of it.
//
// One directory is never a source: clusters/<cluster>/catalogue, where the
// director itself commits the profiles tenants installed. A catalogue there
// would list as installable what somebody installed from another catalogue
// -- a tenant's own among them -- and record it as coming from here.

// ErrCatalogueDirectoryRefused is a directory, or a file of one, that no
// catalogue is read from.
var ErrCatalogueDirectoryRefused = errors.New("catalogue: the directory is refused")

// maxCatalogueDirectory bounds a declared directory. It is written into a
// manifest and joined to a path.
const maxCatalogueDirectory = 255

func refusedDirectory(why string) error {
	return fmt.Errorf("%w: %s", ErrCatalogueDirectoryRefused, why)
}

// checkRepositoryPath is what any path read for a catalogue must be: names
// separated by single slashes, each of letters, digits, '.', '_' and '-', and
// none beginning with a dot -- which is ".", "..", ".git" and every hidden
// file at once.
func checkRepositoryPath(path string) error {
	if path == "" {
		return refusedDirectory("a directory is required")
	}
	if len(path) > maxCatalogueDirectory {
		return refusedDirectory(fmt.Sprintf("it is longer than %d characters", maxCatalogueDirectory))
	}
	if strings.HasPrefix(path, "/") {
		return refusedDirectory("it is an absolute path; a directory is named from the top of the deployments repository, as in catalogue or catalogues/acme")
	}
	for _, c := range path {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._/", c) {
			return refusedDirectory(fmt.Sprintf("it contains %q; a directory is names of letters, digits, '.', '_' and '-', separated by '/'", c))
		}
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			return refusedDirectory("it has an empty name in it; write it without a leading, trailing or doubled '/'")
		}
		if strings.HasPrefix(part, ".") {
			return refusedDirectory(fmt.Sprintf("it names %q; no part of it may begin with a dot, which rules out '.', '..' and hidden directories", part))
		}
	}
	return nil
}

// CheckCatalogueDirectory says what is wrong with a directory of the
// deployments repository as a catalogue's, as it is written, or nil. It
// reads nothing.
func CheckCatalogueDirectory(dir string) error {
	if err := checkRepositoryPath(dir); err != nil {
		return err
	}
	// Any cluster's, not only this one's: a deployments repository may hold
	// several clusters, and what another cluster's tenants installed is no
	// more a catalogue than what this one's did. Compared without regard to
	// case, so that the answer does not depend on the file system.
	parts := strings.Split(strings.ToLower(dir), "/")
	if len(parts) >= 3 && parts[0] == "clusters" && parts[2] == CatalogueDir {
		return refusedDirectory("clusters/<cluster>/" + CatalogueDir + " is where the director writes the profiles tenants installed, and nothing in it is a catalogue's")
	}
	return nil
}

// plainPath walks from the top of the checkout to a path, one name at a time,
// and returns where it is on disk. Every name on the way must be a directory
// and the last what wantDir says; a symbolic link anywhere is refused, as is
// anything that is neither a file nor a directory. What does not exist is
// fs.ErrNotExist.
//
// The caller holds the lock. Nothing but this process writes the checkout,
// and it does so under the same lock, so what is looked at here is what is
// opened next.
func (g *GitOps) plainPath(rel string, wantDir bool) (string, error) {
	at := g.path
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		if err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
			}
			return "", err
		}
		shown := strings.Join(parts[:i+1], "/")
		last := i == len(parts)-1
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return "", refusedDirectory(shown + " is a symbolic link, and a catalogue in the deployments repository is plain directories and files")
		case info.IsDir():
			if last && !wantDir {
				return "", refusedDirectory(shown + " is a directory where a file is expected")
			}
		case info.Mode().IsRegular():
			if !last {
				// A file where a directory would have to be: nothing is below it.
				return "", fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
			}
			if wantDir {
				return "", refusedDirectory(shown + " is a file, not a directory")
			}
		default:
			return "", refusedDirectory(shown + " is neither a file nor a directory")
		}
	}
	return at, nil
}

// catalogueDirectoryPresent refuses a directory that is not written as one a
// catalogue is read from, or is not a plain directory of the repository as
// the remote has it now: what a person is told when a catalogue is added,
// rather than at the first install.
func (g *GitOps) catalogueDirectoryPresent(ctx context.Context, dir string) error {
	if err := CheckCatalogueDirectory(dir); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}
	if _, err := g.plainPath(dir, true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return refusedDirectory("the deployments repository has no directory " + dir)
		}
		return err
	}
	return nil
}

// CatalogueFile reads one file of a catalogue kept in the deployments
// repository: file is index.yaml or profiles/<name>.yaml, below dir.
//
// It is read from the checkout, at the commit the checkout is at. At most
// limit+1 bytes are returned, so that the caller can tell a file that is too
// large from one that fits. A file that is not there is fs.ErrNotExist; a
// directory or a file no catalogue is read from is
// ErrCatalogueDirectoryRefused.
func (g *GitOps) CatalogueFile(ctx context.Context, dir, file string, limit int) ([]byte, error) {
	if err := CheckCatalogueDirectory(dir); err != nil {
		return nil, err
	}
	if err := checkRepositoryPath(file); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return nil, err
	}
	path, err := g.plainPath(dir+"/"+file, false)
	if err != nil {
		return nil, err
	}
	// Not followed here either, should the last name have become a link
	// between the look and the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, int64(limit)+1))
}
