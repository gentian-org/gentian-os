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

package controller_test

import (
	"context"
	"encoding/json"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const tenantProvisioningJobsConfigType = "tenant-provisioning-jobs"

// startTenantProvisioningJobSimulator creates Batch Jobs and other K8s objects from the operator's
// tenant-*-provisioning-jobs ConfigMap. Envtest has no Crossplane; this mimics
// tenant-default Job/Object MRs for controller integration tests.
func startTenantProvisioningJobSimulator(ctx context.Context, c client.Client) {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				simulateTenantProvisioningJobsOnce(ctx, c)
			}
		}
	}()
}

func simulateTenantProvisioningJobsOnce(ctx context.Context, c client.Client) {
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms, client.InNamespace("platform-kernel"),
		client.MatchingLabels{"gentianos.io/config-type": tenantProvisioningJobsConfigType}); err != nil {
		return
	}
	for i := range cms.Items {
		cm := &cms.Items[i]
		payload := cm.Data["jobs.json"]
		if payload == "" {
			continue
		}
		var jobs []batchv1.Job
		if err := json.Unmarshal([]byte(payload), &jobs); err != nil {
			continue
		}
		for j := range jobs {
			job := jobs[j]
			if job.Namespace == "" {
				job.Namespace = "platform-kernel"
			}
			createIfMissing(ctx, c, &job)
		}

		if !provisioningJobsComplete(ctx, c, jobs) {
			continue
		}

		objectsPayload := cm.Data["objects.json"]
		if objectsPayload == "" {
			continue
		}
		var rawObjects []json.RawMessage
		if err := json.Unmarshal([]byte(objectsPayload), &rawObjects); err != nil {
			continue
		}
		for _, raw := range rawObjects {
			obj := &unstructured.Unstructured{}
			if err := json.Unmarshal(raw, &obj.Object); err != nil {
				continue
			}
			if obj.GetNamespace() == "" {
				obj.SetNamespace("platform-kernel")
			}
			createIfMissing(ctx, c, obj)
			patchSimulatorObjectStatus(ctx, c, obj)
		}
	}
}

func provisioningJobsComplete(ctx context.Context, c client.Client, jobs []batchv1.Job) bool {
	for i := range jobs {
		job := jobs[i]
		ns := job.Namespace
		if ns == "" {
			ns = "platform-kernel"
		}
		current := &batchv1.Job{}
		if err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: ns}, current); err != nil {
			return false
		}
		if current.Status.Succeeded == 0 {
			return false
		}
	}
	return true
}

func patchSimulatorObjectStatus(ctx context.Context, c client.Client, obj *unstructured.Unstructured) {
	switch obj.GetKind() {
	case "Deployment":
		patchDeploymentReady(ctx, c, obj.GetNamespace(), obj.GetName())
	case "Gateway":
		patchGatewayProgrammed(ctx, c, obj.GetNamespace(), obj.GetName())
	case "HTTPRoute":
		patchHTTPRouteProgrammed(ctx, c, obj.GetNamespace(), obj.GetName())
	case "Database":
		patchDatabaseCRApplied(ctx, c, obj.GetNamespace(), obj.GetName())
	}
}

func patchDatabaseCRApplied(ctx context.Context, c client.Client, namespace, name string) {
	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"})
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, db); err != nil {
		return
	}
	applied, found, _ := unstructured.NestedBool(db.Object, "status", "applied")
	if found && applied {
		return
	}
	_ = unstructured.SetNestedField(db.Object, true, "status", "applied")
	_ = c.Status().Update(ctx, db)
}

func patchDeploymentReady(ctx context.Context, c client.Client, namespace, name string) {
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err != nil {
		return
	}
	if dep.Status.ReadyReplicas > 0 {
		return
	}
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	dep.Status.ReadyReplicas = replicas
	dep.Status.AvailableReplicas = replicas
	dep.Status.UpdatedReplicas = replicas
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
	}
	_ = c.Status().Update(ctx, dep)
}

func patchGatewayProgrammed(ctx context.Context, c client.Client, namespace, name string) {
	gw := &gatewayv1.Gateway{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, gw); err != nil {
		return
	}
	for _, cond := range gw.Status.Conditions {
		if cond.Type == string(gatewayv1.GatewayConditionProgrammed) && cond.Status == metav1.ConditionTrue {
			return
		}
	}
	now := metav1.Now()
	gw.Status.Conditions = []metav1.Condition{
		{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, Reason: "Programmed", LastTransitionTime: now},
	}
	for i := range gw.Spec.Listeners {
		gw.Status.Listeners = append(gw.Status.Listeners, gatewayv1.ListenerStatus{
			Name: gw.Spec.Listeners[i].Name,
			Conditions: []metav1.Condition{
				{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, Reason: "Programmed", LastTransitionTime: now},
			},
		})
	}
	_ = c.Status().Update(ctx, gw)
}

func patchHTTPRouteProgrammed(ctx context.Context, c client.Client, namespace, name string) {
	route := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, route); err != nil {
		return
	}
	for _, cond := range route.Status.Parents {
		for _, parentCond := range cond.Conditions {
			if parentCond.Type == string(gatewayv1.RouteConditionAccepted) && parentCond.Status == metav1.ConditionTrue {
				return
			}
		}
	}
	now := metav1.Now()
	route.Status.Parents = []gatewayv1.RouteParentStatus{
		{
			Conditions: []metav1.Condition{
				{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: now},
				{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue, Reason: "ResolvedRefs", LastTransitionTime: now},
			},
		},
	}
	_ = c.Status().Update(ctx, route)
}
