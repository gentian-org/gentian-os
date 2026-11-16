/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package dispatcher

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Follow keeps the server's table the one the operator last wrote: the file
// is read at every interval, and a change that parses replaces the table. A
// file that is not there is an empty table, under which every key is
// refused; one that does not parse leaves the last table in place.
func Follow(ctx context.Context, path string, interval time.Duration, s *Server, log *slog.Logger) {
	var last []byte
	read := func() {
		raw, err := os.ReadFile(path) //nolint:gosec // The operator's table, at the path the chart mounts it.
		if err != nil {
			if os.IsNotExist(err) && s.Table() == nil {
				s.SetTable(&Table{})
			}
			return
		}
		if bytes.Equal(raw, last) {
			return
		}
		table, err := ParseTable(raw)
		if err != nil {
			log.Error("the table is not one; the last one stays", "error", err.Error())
			return
		}
		last = raw
		s.SetTable(table)
		log.Info("table read", "callers", len(table.Callers), "tenants", len(table.Tenants))
	}
	read()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			read()
		}
	}
}

// KeepSwept sweeps finished runs at every interval until ctx ends.
func KeepSwept(ctx context.Context, pods PodClient, s *Server, keep, interval time.Duration, log *slog.Logger) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if swept, err := Sweep(ctx, pods, s.Table(), keep, now); err != nil {
				log.Error("sweeping finished runs", "error", err.Error())
			} else if swept > 0 {
				log.Info("finished runs swept", "pods", swept)
			}
		}
	}
}

// ClusterPods is the PodClient at a cluster.
type ClusterPods struct{ Client kubernetes.Interface }

func (c ClusterPods) Create(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	return c.Client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
}

func (c ClusterPods) Get(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return c.Client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c ClusterPods) List(ctx context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error) {
	list, err := c.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(selector).String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c ClusterPods) Delete(ctx context.Context, namespace, name string) error {
	return c.Client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// Logs is the end of a container's log, at most limit bytes of it.
func (c ClusterPods) Logs(ctx context.Context, namespace, name, container string, limit int64) (string, error) {
	stream, err := c.Client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{Container: container, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	raw, err := io.ReadAll(io.LimitReader(stream, limit))
	return string(raw), err
}
