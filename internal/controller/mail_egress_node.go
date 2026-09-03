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

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mailEgressNodeLabel marks the node mail must leave from.
//
// The Postfix chart already selects on it — see kernel/services/postfix, whose
// values carry egressNodeLabel and whose nodeSelector renders only when the
// cluster names an egressHost. Nothing set it, so that selector matched no node:
// a cluster that configured a dedicated egress would have had Postfix pinned to
// a label that did not exist, and the Pod would sit Pending rather than send
// from the wrong address. Unschedulable is the safer failure of the two, but it
// is still a failure, and it arrives at the next reschedule rather than at
// configuration time.
const mailEgressNodeLabel = "gentianos.io/mail-egress"

// syncMailEgressNodeLabel puts the label on the node carrying the floating IP
// and takes it off every other node.
//
// The node is identified by its ExternalIP, which the cloud provider reports
// once the address is attached to that node's port — the same signal
// mailEgressAddress publishes the A record from, so the label and the record
// cannot disagree about which node is the egress.
//
// A cluster with no dedicated egress labels nothing: the chart renders no
// nodeSelector in that case, so scheduling stays free and this is a no-op.
//
// Errors are returned but callers log rather than fail on them. Being unable to
// label a node should not stop a tenant reconcile; the consequence is that
// Postfix may schedule elsewhere, which is what happens today anyway.
func (r *TenantReconciler) syncMailEgressNodeLabel(ctx context.Context) error {
	egress := clusterMailEgressHost(ctx, r.Client, envOrDefault("MAIL_EGRESS_HOST", ""))
	if egress == "" {
		return nil
	}
	want := r.mailEgressAddress(ctx)
	if want == "" {
		return nil
	}

	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return err
	}
	log := ctrl.LoggerFrom(ctx)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		carries := false
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeExternalIP && a.Address == want {
				carries = true
				break
			}
		}
		_, labelled := node.Labels[mailEgressNodeLabel]
		if carries == labelled {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		if carries {
			if node.Labels == nil {
				node.Labels = map[string]string{}
			}
			node.Labels[mailEgressNodeLabel] = "true"
		} else {
			delete(node.Labels, mailEgressNodeLabel)
		}
		if err := r.Patch(ctx, node, patch); err != nil {
			return err
		}
		log.Info("mail egress node label updated",
			"node", node.Name, "labelled", carries, "address", want)
	}
	return nil
}
