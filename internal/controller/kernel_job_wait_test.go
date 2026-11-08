/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProvisioningJobObjectName(t *testing.T) {
	t.Parallel()
	got := provisioningJobObjectName("demo", "keycloak-gentian-groups-demo")
	if got != "demo-job-keycloak-gentian-groups-demo" {
		t.Fatalf("got %q", got)
	}
}

func TestCrossplaneObjectReady(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": string(metav1.ConditionTrue),
				},
			},
		},
	})
	if !crossplaneObjectReady(obj) {
		t.Fatal("expected Object Ready=True")
	}
	obj.SetUnstructuredContent(map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": string(metav1.ConditionFalse),
				},
			},
		},
	})
	if crossplaneObjectReady(obj) {
		t.Fatal("expected Object Ready=False")
	}
}
