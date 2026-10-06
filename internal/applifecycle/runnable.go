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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// Runnable serves the app lifecycle HTTP API inside the operator manager.
type Runnable struct {
	Server *HTTPServer
	// reader and writer reach the API server for the one object this
	// runnable owns: the Secret the usher's token is handed over in.
	reader client.Reader
	writer client.Client
	// readTokenSecret names that Secret, in the edge namespace. Empty on a
	// cluster with no usher: the listener then has no reader at all.
	readTokenSecret string
}

// NewRunnableFromEnv builds the lifecycle HTTP server from operator environment.
func NewRunnableFromEnv(mgr manager.Manager) (*Runnable, error) {
	addr := os.Getenv("APP_LIFECYCLE_BIND_ADDRESS")
	if addr == "" {
		addr = ":8082"
	}
	svc, err := NewService(mgr.GetClient(), mgr.GetConfig(), Options{
		OpenBaoNamespace:  envOrDefault("OPENBAO_NAMESPACE", "openbao"),
		OperatorNamespace: envOrDefault("POD_NAMESPACE", layout.Namespace(layout.Control)),
		OperatorSA:        envOrDefault("OPERATOR_SA", "gentian-os"),
		MetricsEnabled:    os.Getenv("METRICS_SERVER_ENABLED") == "true",
	})
	if err != nil {
		return nil, err
	}
	// The shared token the director presents. Absent means the server
	// admits nobody as the director: an operator whose Secret failed to mount
	// must not fall back to the open API this replaced.
	return &Runnable{
		Server: &HTTPServer{
			Service: svc,
			Addr:    addr,
			Token:   os.Getenv("APP_LIFECYCLE_TOKEN"),
		},
		// Read uncached: the manager's cache would start watching every
		// Secret in the cluster to answer for this one.
		reader:          mgr.GetAPIReader(),
		writer:          mgr.GetClient(),
		readTokenSecret: os.Getenv("APP_LIFECYCLE_READ_TOKEN_SECRET"),
	}, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Start implements manager.Runnable.
func (r *Runnable) Start(ctx context.Context) error {
	if r.readTokenSecret != "" {
		go r.handOverReadToken(ctx)
	}
	return r.Server.Start(ctx)
}

// readTokenKey is the key the token is stored under.
const readTokenKey = "token"

// handOverReadToken makes sure the usher's token exists and tells the
// listener what it is, and keeps doing so.
//
// The operator mints the token and writes it where the usher mounts it,
// rather than the chart rendering one value into two namespaces: one writer
// cannot disagree with itself, and a token that exists only as this Secret is
// one no template, values file or repository ever held. Until it succeeds the
// listener has no reader and the usher's reads of live state fail, which is
// the safe direction. It is looked at again every few minutes, so a Secret
// somebody deleted comes back and both sides follow the new value.
func (r *Runnable) handOverReadToken(ctx context.Context) {
	log := ctrllog.FromContext(ctx).WithName("app-lifecycle")
	for {
		wait := 5 * time.Minute
		token, err := ensureReadToken(ctx, r.reader, r.writer, layout.Namespace(layout.Edge), r.readTokenSecret)
		if err != nil {
			log.Error(err, "the usher's token could not be handed over; its reads of live state are refused until it is")
			wait = 15 * time.Second
		} else {
			r.Server.SetReadToken(token)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ensureReadToken returns the token in the named Secret, creating the Secret
// with a fresh one when there is none. A token that exists is kept: every
// replica of the operator must admit the same one, and the usher reads it
// from the same place.
func ensureReadToken(ctx context.Context, reader client.Reader, writer client.Client, namespace, name string) (string, error) {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	for attempt := 0; attempt < 3; attempt++ {
		var secret corev1.Secret
		err := reader.Get(ctx, key, &secret)
		switch {
		case err == nil:
			if token := string(secret.Data[readTokenKey]); token != "" {
				return token, nil
			}
			token, err := newReadToken()
			if err != nil {
				return "", err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data[readTokenKey] = []byte(token)
			if err := writer.Update(ctx, &secret); err != nil {
				if apierrors.IsConflict(err) {
					continue
				}
				return "", fmt.Errorf("write %s/%s: %w", namespace, name, err)
			}
			return token, nil
		case apierrors.IsNotFound(err):
			token, err := newReadToken()
			if err != nil {
				return "", err
			}
			secret = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace, Name: name,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "gentian-os",
						"app.kubernetes.io/component":  "usher",
					},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{readTokenKey: []byte(token)},
			}
			if err := writer.Create(ctx, &secret); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Another replica got there first; its token is the one.
					continue
				}
				return "", fmt.Errorf("create %s/%s: %w", namespace, name, err)
			}
			return token, nil
		default:
			return "", fmt.Errorf("read %s/%s: %w", namespace, name, err)
		}
	}
	return "", fmt.Errorf("%s/%s kept changing under this operator", namespace, name)
}

// newReadToken is 32 random bytes, in hex.
func newReadToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint a token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// NeedLeaderElection returns false so any operator replica can serve read-mostly API calls.
func (r *Runnable) NeedLeaderElection() bool { return false }
