/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package crdcheck

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// HoldRequeue is how soon a held object is looked at again. It matches the
// checker's retry, so an object resumes within about a minute of the
// definitions being brought up to date and without anything touching it.
const HoldRequeue = DefaultRetryInterval

// pendingRequeue is how soon an object is looked at again while the first
// check has not completed.
const pendingRequeue = 2 * time.Second

// Holder is what a reconciler is given so that it can be held: the gate and
// where to record that it was. A nil *Holder holds nothing -- a reconciler
// built without one, as every unit test builds them, runs as it always did.
type Holder struct {
	Gate *schemacheck.Gate
	// Set is the definitions the gate reports on. Nil is the embedded set.
	Set *schemacheck.Set
	// Recorder records the hold on the held object. Nil records nothing.
	Recorder record.EventRecorder
}

func (h *Holder) set() *schemacheck.Set {
	if h.Set != nil {
		return h.Set
	}
	return schemacheck.Embedded()
}

// Guard wraps a reconciler of primary so that it does nothing while a
// definition it writes into would drop fields.
//
// What it depends on is every definition the chart delivers, and the kinds
// named in generated beside them. The first is not narrowed per reconciler on
// purpose: each of these reconcilers writes its own kind's status and
// several other chart kinds besides, which of them is not something a list
// kept here would stay right about, and the chart's definitions reach a
// cluster together or not at all. generated names the Crossplane kinds the
// reconciler writes, which arrive by another road (installer step B-06) and
// go stale on their own.
//
// Until the cluster has been read once the reconciler is not called either,
// and the request simply comes back in a moment.
//
// While held, the reconciler is not called at all: the object gets the
// DefinitionsCurrent condition, False, with the reason and a message naming
// the definition, the fields and the remedy; an Event says the same once;
// and the request comes back after HoldRequeue. When the gate lets go the
// condition is taken off and the reconciler runs. Nothing is remembered in
// between, so a restarted operator clears a condition it finds as readily as
// one it set.
func (h *Holder) Guard(c client.Client, primary client.Object, inner reconcile.Reconciler, generated ...string) reconcile.Reconciler {
	if h == nil || h.Gate == nil {
		return inner
	}
	set := h.set()
	selectors := []schemacheck.Selector{schemacheck.Chart}
	if len(generated) > 0 {
		selectors = append(selectors, schemacheck.Kinds(set, generated...))
	}
	return &guard{holder: h, client: c, primary: primary, inner: inner, selectors: selectors}
}

type guard struct {
	holder    *Holder
	client    client.Client
	primary   client.Object
	inner     reconcile.Reconciler
	selectors []schemacheck.Selector
}

func (g *guard) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	hold := g.holder.Gate.Hold(g.selectors...)
	if hold == nil {
		if g.release(ctx, req) {
			// The reconciler reads the object from a cache that has not seen
			// the condition go yet, and would write it straight back with
			// its own status. One more pass, a moment later.
			return reconcile.Result{RequeueAfter: time.Second}, nil
		}
		return g.inner.Reconcile(ctx, req)
	}
	if hold.Pending {
		// The operator has only just started and has not read the cluster
		// yet, which takes a moment. Nothing is written, the object included:
		// marking every object held at every start, to unmark it a second
		// later, would be a thousand writes that say nothing.
		return reconcile.Result{RequeueAfter: pendingRequeue}, nil
	}
	g.mark(ctx, req, hold)
	return reconcile.Result{RequeueAfter: HoldRequeue}, nil
}

// object reads the object a request names, from the cache the wrapped
// reconciler reads it from. Nil when there is none: a request that names no
// object of the primary kind (a reconciler of one fixed thing) is held all
// the same, and simply has nowhere to carry the condition.
func (g *guard) object(ctx context.Context, req reconcile.Request) client.Object {
	obj, ok := g.primary.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	if err := g.client.Get(ctx, req.NamespacedName, obj); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("held object could not be read", "error", err.Error())
		}
		return nil
	}
	return obj
}

// conditions reads status.conditions off any of the operator's kinds. Every
// one of them has the field, and going through the unstructured form is what
// lets one guard serve them all without each type implementing an interface.
func conditions(obj client.Object) ([]metav1.Condition, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	status, _ := raw["status"].(map[string]interface{})
	list, ok := status["conditions"]
	if !ok {
		return nil, nil
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	var out []metav1.Condition
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// write replaces status.conditions. A merge patch of the status subresource
// carrying only that list, so nothing else of the object is sent -- which
// matters here more than anywhere: the point of holding is to not write the
// object through a definition that drops fields.
func (g *guard) write(ctx context.Context, obj client.Object, conds []metav1.Condition) error {
	if conds == nil {
		conds = []metav1.Condition{}
	}
	patch, err := json.Marshal(map[string]interface{}{"status": map[string]interface{}{"conditions": conds}})
	if err != nil {
		return err
	}
	return g.client.Status().Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

func (g *guard) mark(ctx context.Context, req reconcile.Request, hold *schemacheck.Hold) {
	logger := log.FromContext(ctx)
	obj := g.object(ctx, req)
	if obj == nil {
		logger.Info("held: nothing is written while a definition would drop fields",
			"reason", hold.Reason, "message", hold.Message)
		return
	}
	conds, err := conditions(obj)
	if err != nil {
		logger.Error(err, "held, and the object's conditions could not be read")
		return
	}
	current := meta.FindStatusCondition(conds, schemacheck.ConditionType)
	if current != nil && current.Status == metav1.ConditionFalse && current.Reason == hold.Reason && current.Message == hold.Message {
		return
	}
	meta.SetStatusCondition(&conds, metav1.Condition{
		Type:               schemacheck.ConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             hold.Reason,
		Message:            hold.Message,
		ObservedGeneration: obj.GetGeneration(),
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
	if err := g.write(ctx, obj, conds); err != nil {
		// The hold stands whether or not it could be written down.
		logger.Error(err, "held, and the condition saying so could not be written",
			"reason", hold.Reason, "message", hold.Message)
		return
	}
	logger.Info("held: nothing is written while a definition would drop fields",
		"reason", hold.Reason, "message", hold.Message)
	if g.holder.Recorder != nil {
		g.holder.Recorder.Event(obj, corev1.EventTypeWarning, hold.Reason, hold.Message)
	}
}

// release takes the condition off an object that is no longer held, and
// reports whether there was one to take off.
func (g *guard) release(ctx context.Context, req reconcile.Request) bool {
	obj := g.object(ctx, req)
	if obj == nil {
		return false
	}
	conds, err := conditions(obj)
	if err != nil || meta.FindStatusCondition(conds, schemacheck.ConditionType) == nil {
		return false
	}
	meta.RemoveStatusCondition(&conds, schemacheck.ConditionType)
	if err := g.write(ctx, obj, conds); err != nil {
		// Not worth holding the object for: the reconciler runs, and the
		// next pass tries again.
		log.FromContext(ctx).Error(err, "no longer held, and the condition saying it was could not be removed")
		return false
	}
	if g.holder.Recorder != nil {
		g.holder.Recorder.Event(obj, corev1.EventTypeNormal, "DefinitionsCurrent",
			"the cluster's definitions are current again; reconciling resumes")
	}
	return true
}
