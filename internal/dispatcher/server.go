/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/gentian-org/gentian-os/internal/audit"
)

// PodClient is what the dispatcher does at the Kubernetes API: pods of the
// namespaces for runs, and nothing else.
type PodClient interface {
	Create(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
	Get(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	List(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error)
	Delete(ctx context.Context, namespace, name string) error
	Logs(ctx context.Context, namespace, name, container string, limit int64) (string, error)
}

// MaxLog is the most of a run's log a caller is handed.
const MaxLog = 64 * 1024

// Server answers the components that start runs.
//
// It keeps nothing: who may start what is the operator's table, read anew
// whenever it changes, and a run is its pod. A caller is known by its key
// alone, and the tenant and the component are the table's for that key,
// never anything a request says. Who a run is for is the caller's claim and
// is recorded as one; nothing here verifies it, which holds only because no
// one reaches this listener but the components the table names.
type Server struct {
	pods         PodClient
	programImage string
	log          *slog.Logger
	table        atomic.Pointer[Table]
}

// NewServer returns a server that starts runs with the run program of
// programImage. Until a table is set, every key is refused.
func NewServer(pods PodClient, programImage string, log *slog.Logger) *Server {
	return &Server{pods: pods, programImage: programImage, log: log}
}

// SetTable replaces the table the server decides by.
func (s *Server) SetTable(t *Table) { s.table.Store(t) }

// Table is the table the server decides by, nil before the first.
func (s *Server) Table() *Table { return s.table.Load() }

// Handler is the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /v1/runs", s.as(s.start))
	mux.HandleFunc("GET /v1/runs", s.as(s.list))
	mux.HandleFunc("GET /v1/runs/{id}", s.as(s.read))
	mux.HandleFunc("DELETE /v1/runs/{id}", s.as(s.cancel))
	mux.HandleFunc("GET /v1/runs/{id}/logs", s.as(s.logs))
	mux.HandleFunc("GET /v1/actions", s.as(s.actions))
	return mux
}

type call struct {
	table  *Table
	caller *Caller
	tenant *Tenant
}

func (s *Server) answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) refuse(w http.ResponseWriter, status int, message string) {
	s.answer(w, status, map[string]string{"error": message})
}

func (s *Server) record(r *http.Request, c *call, action, object, outcome string, detail map[string]any) {
	rec := audit.Record{
		Source: "dispatcher", Kind: audit.KindChange, RequestID: r.Header.Get("X-Request-Id"),
		Object: object, Action: action, Outcome: outcome, Detail: detail,
	}
	if c != nil {
		rec.Tenant, rec.Subject = c.caller.Tenant, "component:"+c.caller.Tenant+"/"+c.caller.Component
	}
	audit.Write(r.Context(), s.log, rec)
}

// as finds the caller a request's key belongs to, and refuses a request
// without one before anything of it is read.
func (s *Server) as(next func(http.ResponseWriter, *http.Request, *call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		table := s.table.Load()
		caller := table.CallerByKey(strings.TrimSpace(key))
		if !bearer || caller == nil {
			s.record(r, nil, r.Method+" "+r.URL.Path, "", audit.Refused, map[string]any{"reason": "no key the table names"})
			s.refuse(w, http.StatusUnauthorized, "the key is not one the dispatcher knows")
			return
		}
		tenant := table.TenantOf(caller.Tenant)
		if tenant == nil {
			s.refuse(w, http.StatusServiceUnavailable, "the tenant has no namespace for runs yet")
			return
		}
		next(w, r, &call{table: table, caller: caller, tenant: tenant})
	}
}

func validID(id string) bool {
	if id == "" || len(id) > MaxID {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || r == '/' {
			return false
		}
	}
	return true
}

type startRequest struct {
	ID              string `json:"id"`
	Action          string `json:"action"`
	DeadlineSeconds int32  `json:"deadlineSeconds"`
	CallbackToken   string `json:"callbackToken"`
	Person          string `json:"person"`
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, c *call) {
	var req startRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		s.refuse(w, http.StatusBadRequest, "the body is not a run to start: "+err.Error())
		return
	}
	object := "run:" + c.caller.Tenant + "/" + c.caller.Component + "/" + req.ID
	refused := func(status int, message string) {
		s.record(r, c, "start", object, audit.Refused, map[string]any{"action": req.Action, "reason": message, "person": req.Person})
		s.refuse(w, status, message)
	}
	if !validID(req.ID) {
		refused(http.StatusUnprocessableEntity, "a run needs an id of at most 253 characters, without a slash or a control character")
		return
	}
	if req.CallbackToken == "" || len(req.CallbackToken) > 4096 || len(req.Person) > 253 {
		refused(http.StatusUnprocessableEntity, "a run needs a callbackToken of at most 4096 characters, and a person of at most 253")
		return
	}
	runnable, own, found := c.table.Runnable(c.caller, req.Action)
	if !found {
		refused(http.StatusNotFound, "nothing called "+req.Action+" is installed in this tenant or declared as a run of this component")
		return
	}
	if req.DeadlineSeconds == 0 {
		req.DeadlineSeconds = runnable.DeadlineSeconds
	}
	if req.DeadlineSeconds < 1 || req.DeadlineSeconds > runnable.DeadlineSeconds {
		refused(http.StatusUnprocessableEntity, "the deadline is longer than "+req.Action+" may take")
		return
	}

	hash := Hash(c.caller.Tenant, c.caller.Component, req.ID)
	// A run that exists is returned as it is: asking twice starts one.
	if existing, err := s.pods.List(r.Context(), c.tenant.Namespace, map[string]string{LabelRun: hash}); err != nil {
		s.failed(w, r, err)
		return
	} else if len(existing) > 0 {
		s.answer(w, http.StatusOK, RunOf(latest(existing)[0]))
		return
	}
	mine, err := s.pods.List(r.Context(), c.tenant.Namespace, map[string]string{LabelCaller: c.caller.Component})
	if err != nil {
		s.failed(w, r, err)
		return
	}
	working := int32(0)
	for i := range mine {
		if finishedAt(&mine[i]).IsZero() {
			working++
		}
	}
	if working >= c.caller.Concurrency {
		refused(http.StatusTooManyRequests, "this component has as many runs under way as it may have at once")
		return
	}

	pod := Pod(c.tenant, c.caller, runnable, own, Start{
		ID: req.ID, Deadline: req.DeadlineSeconds, Token: req.CallbackToken, Person: req.Person, Attempt: 1,
	}, s.programImage)
	created, err := s.pods.Create(r.Context(), pod)
	if apierrors.IsAlreadyExists(err) {
		if created, err = s.pods.Get(r.Context(), pod.Namespace, pod.Name); err == nil {
			s.answer(w, http.StatusOK, RunOf(created))
			return
		}
	}
	if err != nil {
		// What the cluster refuses, a quota that is full among it, is said
		// to the caller as the cluster said it.
		status := http.StatusBadGateway
		if apierrors.IsForbidden(err) {
			status = http.StatusTooManyRequests
		}
		s.record(r, c, "start", object, audit.Failed, map[string]any{"action": req.Action, "error": err.Error(), "person": req.Person})
		s.refuse(w, status, "the run could not be started: "+err.Error())
		return
	}
	s.record(r, c, "start", object, audit.OK, map[string]any{
		"action": req.Action, "own": own, "image": runnable.Ref(), "pod": created.Name, "person": req.Person,
	})
	s.answer(w, http.StatusCreated, RunOf(created))
}

func (s *Server) failed(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "the cluster did not answer", "error", err.Error())
	s.refuse(w, http.StatusBadGateway, "the cluster did not answer: "+err.Error())
}

// podsOf are the pods of the caller's run of that id, newest attempt last.
func (s *Server) podsOf(r *http.Request, c *call) ([]corev1.Pod, error) {
	id := r.PathValue("id")
	if !validID(id) {
		return nil, errors.New("no such run")
	}
	return s.pods.List(r.Context(), c.tenant.Namespace, map[string]string{LabelRun: Hash(c.caller.Tenant, c.caller.Component, id)})
}

const gone = "no such run: a run's pod is kept ten minutes after it ends, and the record of it is its caller's"

func (s *Server) read(w http.ResponseWriter, r *http.Request, c *call) {
	pods, err := s.podsOf(r, c)
	if err != nil || len(pods) == 0 {
		s.refuse(w, http.StatusNotFound, gone)
		return
	}
	s.answer(w, http.StatusOK, RunOf(latest(pods)[0]))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, c *call) {
	pods, err := s.pods.List(r.Context(), c.tenant.Namespace, map[string]string{LabelCaller: c.caller.Component})
	if err != nil {
		s.failed(w, r, err)
		return
	}
	runs := []Run{}
	for _, pod := range latest(pods) {
		runs = append(runs, RunOf(pod))
	}
	s.answer(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, c *call) {
	pods, err := s.podsOf(r, c)
	if err != nil || len(pods) == 0 {
		s.refuse(w, http.StatusNotFound, gone)
		return
	}
	for i := range pods {
		if err := s.pods.Delete(r.Context(), pods[i].Namespace, pods[i].Name); err != nil && !apierrors.IsNotFound(err) {
			s.failed(w, r, err)
			return
		}
	}
	s.record(r, c, "cancel", "run:"+c.caller.Tenant+"/"+c.caller.Component+"/"+r.PathValue("id"), audit.OK, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, c *call) {
	pods, err := s.podsOf(r, c)
	if err != nil || len(pods) == 0 {
		s.refuse(w, http.StatusNotFound, gone)
		return
	}
	pod := latest(pods)[0]
	text, err := s.pods.Logs(r.Context(), pod.Namespace, pod.Name, Container, MaxLog)
	if err != nil {
		s.refuse(w, http.StatusConflict, "the run has no log yet: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, text)
}

func (s *Server) actions(w http.ResponseWriter, _ *http.Request, c *call) {
	type entry struct {
		Name            string `json:"name"`
		Version         string `json:"version,omitempty"`
		Own             bool   `json:"own"`
		DeadlineSeconds int32  `json:"deadlineSeconds"`
	}
	out := []entry{}
	for _, own := range c.caller.Own {
		out = append(out, entry{Name: own.Name, Version: own.Version, Own: true, DeadlineSeconds: own.DeadlineSeconds})
	}
	for _, action := range c.tenant.Actions {
		out = append(out, entry{Name: action.Name, Version: action.Version, DeadlineSeconds: action.DeadlineSeconds})
	}
	s.answer(w, http.StatusOK, map[string]any{"actions": out})
}
