// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProvisioningJobObjectName(t *testing.T) {
	t.Parallel()
	got := provisioningJobObjectName("demo", "keycloak-ldap-sync-demo")
	if got != "demo-job-keycloak-ldap-sync-demo" {
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
