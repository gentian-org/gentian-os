/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// ConfigMapRecorder keeps the arbiter's records in StoresConfigMap of its
// namespace: one key per store, the ids as JSON. Every change is an update
// against the version read, so two arbiters never write over each other's
// record unseen.
type ConfigMapRecorder struct {
	Client    kubernetes.Interface
	Namespace string
}

func (r *ConfigMapRecorder) read(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := r.Client.CoreV1().ConfigMaps(r.Namespace).Get(ctx, StoresConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: StoresConfigMap, Namespace: r.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": ServiceName},
		}}
		created, err := r.Client.CoreV1().ConfigMaps(r.Namespace).Create(ctx, cm, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return r.Client.CoreV1().ConfigMaps(r.Namespace).Get(ctx, StoresConfigMap, metav1.GetOptions{})
		}
		return created, err
	}
	return cm, err
}

func (r *ConfigMapRecorder) Load(ctx context.Context) (map[string]StoreIDs, error) {
	cm, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]StoreIDs{}
	for key, value := range cm.Data {
		ids, err := decodeIDs(value)
		if err != nil {
			return nil, fmt.Errorf("the record of %s: %w", key, err)
		}
		out[key] = ids
	}
	return out, nil
}

// change applies edit to the record and writes it, again on a conflict.
// edit answers whether there is anything to write.
func (r *ConfigMapRecorder) change(ctx context.Context, edit func(data map[string]string) (bool, error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := r.read(ctx)
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		changed, err := edit(cm.Data)
		if err != nil || !changed {
			return err
		}
		_, err = r.Client.CoreV1().ConfigMaps(r.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (r *ConfigMapRecorder) Claim(ctx context.Context, key string, ids StoreIDs) (StoreIDs, error) {
	recorded := ids
	err := r.change(ctx, func(data map[string]string) (bool, error) {
		if v, ok := data[key]; ok {
			existing, err := decodeIDs(v)
			if err != nil {
				return false, err
			}
			recorded = existing
			return false, nil
		}
		v, err := encodeIDs(ids)
		data[key] = v
		recorded = ids
		return true, err
	})
	return recorded, err
}

func (r *ConfigMapRecorder) Save(ctx context.Context, key string, ids StoreIDs) error {
	return r.change(ctx, func(data map[string]string) (bool, error) {
		v, err := encodeIDs(ids)
		if err != nil || data[key] == v {
			return false, err
		}
		data[key] = v
		return true, nil
	})
}

func (r *ConfigMapRecorder) Forget(ctx context.Context, key string) error {
	return r.change(ctx, func(data map[string]string) (bool, error) {
		if _, ok := data[key]; !ok {
			return false, nil
		}
		delete(data, key)
		return true, nil
	})
}
