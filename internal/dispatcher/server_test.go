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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
)

const (
	flowKey  = "the-key-of-flow"
	otherKey = "the-key-of-another-tenants-flow"
	program  = "registry.example/gentian-os@sha256:" + hash
)

// cluster is the pods of the namespaces for runs, held in memory.
type cluster struct {
	pods    map[string]*corev1.Pod
	refuse  error
	created int
}

func (c *cluster) Create(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if c.refuse != nil {
		return nil, c.refuse
	}
	key := pod.Namespace + "/" + pod.Name
	if c.pods[key] != nil {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, pod.Name)
	}
	kept := pod.DeepCopy()
	kept.CreationTimestamp = metav1.NewTime(time.Unix(int64(1000+c.created), 0))
	kept.Status.Phase = corev1.PodPending
	c.created++
	c.pods[key] = kept
	return kept, nil
}

func (c *cluster) Get(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	if pod := c.pods[namespace+"/"+name]; pod != nil {
		return pod, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

func (c *cluster) List(_ context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error) {
	var out []corev1.Pod
	for _, pod := range c.pods {
		if pod.Namespace == namespace && labels.SelectorFromSet(selector).Matches(labels.Set(pod.Labels)) {
			out = append(out, *pod)
		}
	}
	return out, nil
}

func (c *cluster) Delete(_ context.Context, namespace, name string) error {
	delete(c.pods, namespace+"/"+name)
	return nil
}

func (c *cluster) Logs(_ context.Context, namespace, name, container string, _ int64) (string, error) {
	return "log of " + namespace + "/" + name + "/" + container, nil
}

// end marks a pod's run as over.
func (c *cluster) end(pod *corev1.Pod, phase corev1.PodPhase, code int32, at time.Time) {
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: Container, State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: code, FinishedAt: metav1.NewTime(at)},
	}}}
}

func twoTenants() *Table {
	t := table()
	t.Callers[0].KeyHash = componentkeys.Hash(flowKey)
	other := Caller{Callback: Callback{Service: "flow", Namespace: "tenant-other", Port: 80}, Concurrency: 2}
	other.Tenant, other.Component, other.KeyHash = "other", "flow", componentkeys.Hash(otherKey)
	t.Callers = append(t.Callers, other)
	t.Tenants = append(t.Tenants, Tenant{Tenant: "other", Namespace: "tenant-other-runs", Actions: []Runnable{step("translate")}})
	return &t
}

type harness struct {
	t       *testing.T
	cluster *cluster
	server  *httptest.Server
	audit   *bytes.Buffer
}

func start(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, cluster: &cluster{pods: map[string]*corev1.Pod{}}, audit: &bytes.Buffer{}}
	srv := NewServer(h.cluster, program, slog.New(slog.NewJSONHandler(h.audit, nil)))
	srv.SetTable(twoTenants())
	h.server = httptest.NewServer(srv.Handler())
	t.Cleanup(h.server.Close)
	return h
}

func (h *harness) call(key, method, path, body string) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (h *harness) run(key, id, action string) (int, Run) {
	h.t.Helper()
	status, body := h.call(key, http.MethodPost, "/v1/runs",
		fmt.Sprintf(`{"id":%q,"action":%q,"callbackToken":"token-of-%s","person":"ada"}`, id, action, id))
	var run Run
	_ = json.Unmarshal([]byte(body), &run)
	return status, run
}

// A run is one pod in the caller's tenant's namespace for runs: the action's
// image at its pinned build under the account without rights, labeled for
// the rules that say what it may reach, with the run program around the
// action's command and the caller's address and token in its environment.
func TestARunIsOnePodOfThePinnedImage(t *testing.T) {
	h := start(t)
	status, run := h.run(flowKey, "invoice 7", "ocr")
	if status != http.StatusCreated || run.State != StateWorking || run.ID != "invoice 7" || run.Attempt != 1 {
		t.Fatalf("status %d, run %+v", status, run)
	}
	if len(h.cluster.pods) != 1 {
		t.Fatalf("pods = %v", h.cluster.pods)
	}
	var pod *corev1.Pod
	for _, pod = range h.cluster.pods {
	}
	hashed := Hash("acme", "flow", "invoice 7")
	if pod.Namespace != "tenant-acme-runs" || pod.Name != "run-"+hashed+"-1" {
		t.Fatalf("pod %s/%s", pod.Namespace, pod.Name)
	}
	for label, want := range map[string]string{LabelRun: hashed, LabelCaller: "flow", LabelAction: "ocr", LabelAttempt: "1"} {
		if pod.Labels[label] != want {
			t.Errorf("label %s = %q, want %q", label, pod.Labels[label], want)
		}
	}
	if pod.Annotations[AnnotationPerson] != "ada" || pod.Annotations[AnnotationID] != "invoice 7" {
		t.Errorf("annotations = %v", pod.Annotations)
	}
	spec := pod.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever || *spec.ActiveDeadlineSeconds != 60 || spec.ServiceAccountName != "run" ||
		*spec.AutomountServiceAccountToken || !*spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("spec = %+v", spec)
	}
	main, init := spec.Containers[0], spec.InitContainers[0]
	if main.Image != "registry.example/steps/ocr@sha256:"+hash || init.Image != program ||
		strings.Join(main.Command, " ") != "/gentian-run/run -- /ocr" || strings.Join(init.Command, " ") != "/run-program --install /gentian-run/run" {
		t.Fatalf("containers: %s %v / %s %v", main.Image, main.Command, init.Image, init.Command)
	}
	env := map[string]string{}
	for _, e := range main.Env {
		env[e.Name] = e.Value
	}
	if env[EnvCallbackURL] != "http://flow.tenant-acme.svc:80" || env[EnvToken] != "token-of-invoice 7" || env[EnvID] != "invoice 7" {
		t.Fatalf("env = %v", env)
	}
	if *main.SecurityContext.AllowPrivilegeEscalation || main.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("security context = %+v", main.SecurityContext)
	}

	// Asking again starts nothing, and a run of the caller's own carries no
	// action's label: it is given no action's egress.
	if status, again := h.run(flowKey, "invoice 7", "ocr"); status != http.StatusOK || again.ID != "invoice 7" || len(h.cluster.pods) != 1 {
		t.Fatalf("asked twice: status %d, %d pods", status, len(h.cluster.pods))
	}
	if status, _ := h.run(flowKey, "agent 1", "runner"); status != http.StatusCreated {
		t.Fatalf("own run: %d", status)
	}
	own := h.cluster.pods["tenant-acme-runs/"+PodName(Hash("acme", "flow", "agent 1"), 1)]
	if _, labeled := own.Labels[LabelAction]; labeled || own.Labels[LabelCaller] != "flow" {
		t.Fatalf("own run's labels = %v", own.Labels)
	}
	if !strings.Contains(h.audit.String(), `"action":"start"`) || !strings.Contains(h.audit.String(), `"person":"ada"`) {
		t.Fatalf("no record of the start: %s", h.audit.String())
	}
}

// What is refused: a request without a key the table names, an action that
// is not the tenant's, a deadline longer than the action's, an id that is
// not one, and more runs than the caller may have at once.
func TestWhatTheDispatcherRefuses(t *testing.T) {
	h := start(t)
	for _, key := range []string{"", "not-a-key"} {
		for _, path := range []string{"/v1/runs", "/v1/actions", "/v1/runs/x", "/v1/runs/x/logs"} {
			if status, _ := h.call(key, http.MethodGet, path, ""); status != http.StatusUnauthorized {
				t.Errorf("GET %s with the key %q: %d", path, key, status)
			}
		}
		if status, _ := h.run(key, "a", "ocr"); status != http.StatusUnauthorized {
			t.Errorf("a start with the key %q: %d", key, status)
		}
	}
	if !strings.Contains(h.audit.String(), `"outcome":"refused"`) {
		t.Fatalf("no record of a refusal: %s", h.audit.String())
	}

	// Another tenant's action, and another caller's own run.
	if status, _ := h.run(flowKey, "a", "translate"); status != http.StatusNotFound {
		t.Errorf("another tenant's action: %d", status)
	}
	if status, _ := h.run(otherKey, "a", "runner"); status != http.StatusNotFound {
		t.Errorf("another caller's own run: %d", status)
	}
	for name, body := range map[string]string{
		"a deadline too long": `{"id":"a","action":"ocr","callbackToken":"t","deadlineSeconds":61}`,
		"no token":            `{"id":"a","action":"ocr"}`,
		"no id":               `{"action":"ocr","callbackToken":"t"}`,
		"an id with a slash":  `{"id":"a/b","action":"ocr","callbackToken":"t"}`,
	} {
		if status, _ := h.call(flowKey, http.MethodPost, "/v1/runs", body); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, status)
		}
	}
	if len(h.cluster.pods) != 0 {
		t.Fatalf("a refused run left a pod: %v", h.cluster.pods)
	}

	// Two at once is the caller's limit; a third waits for one to end.
	for _, id := range []string{"1", "2"} {
		if status, _ := h.run(flowKey, id, "ocr"); status != http.StatusCreated {
			t.Fatalf("run %s: %d", id, status)
		}
	}
	if status, _ := h.run(flowKey, "3", "ocr"); status != http.StatusTooManyRequests {
		t.Fatalf("a third run at once: %d", status)
	}
	h.cluster.end(h.cluster.pods["tenant-acme-runs/"+PodName(Hash("acme", "flow", "1"), 1)], corev1.PodSucceeded, 0, time.Now())
	if status, _ := h.run(flowKey, "3", "ocr"); status != http.StatusCreated {
		t.Fatalf("a third run once one ended: %d", status)
	}

	// What the cluster refuses is said as it said it.
	h.cluster.refuse = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", fmt.Errorf("exceeded quota: runs-quota"))
	h.cluster.end(h.cluster.pods["tenant-acme-runs/"+PodName(Hash("acme", "flow", "2"), 1)], corev1.PodSucceeded, 0, time.Now())
	if status, body := h.call(flowKey, http.MethodPost, "/v1/runs", `{"id":"4","action":"ocr","callbackToken":"t"}`); status != http.StatusTooManyRequests || !strings.Contains(body, "runs-quota") {
		t.Fatalf("a full quota: %d %s", status, body)
	}
}

// A caller reads, lists and cancels its own runs and nobody else's, and a
// finished run's pod is swept ten minutes after it ended.
func TestACallersRunsAreItsOwn(t *testing.T) {
	h := start(t)
	h.run(flowKey, "mine", "ocr")
	h.run(otherKey, "mine", "translate")
	h.run(otherKey, "theirs", "translate")

	status, body := h.call(flowKey, http.MethodGet, "/v1/runs", "")
	if status != http.StatusOK || strings.Count(body, `"id"`) != 1 || !strings.Contains(body, `"mine"`) {
		t.Fatalf("list = %d %s", status, body)
	}
	if status, _ := h.call(flowKey, http.MethodGet, "/v1/runs/theirs", ""); status != http.StatusNotFound {
		t.Fatalf("another tenant's run was read: %d", status)
	}
	if status, _ := h.call(flowKey, http.MethodDelete, "/v1/runs/theirs", ""); status != http.StatusNotFound {
		t.Fatalf("another tenant's run was cancelled: %d", status)
	}
	if status, body := h.call(flowKey, http.MethodGet, "/v1/runs/mine/logs", ""); status != http.StatusOK || !strings.Contains(body, "tenant-acme-runs/") {
		t.Fatalf("logs = %d %s", status, body)
	}
	if status, body := h.call(flowKey, http.MethodGet, "/v1/actions", ""); status != http.StatusOK ||
		!strings.Contains(body, `"ocr"`) || !strings.Contains(body, `"runner"`) || strings.Contains(body, "translate") {
		t.Fatalf("actions = %d %s", status, body)
	}

	mine := h.cluster.pods["tenant-acme-runs/"+PodName(Hash("acme", "flow", "mine"), 1)]
	now := time.Now()
	h.cluster.end(mine, corev1.PodFailed, 3, now.Add(-11*time.Minute))
	mine.Status.Reason = "DeadlineExceeded"
	_, body = h.call(flowKey, http.MethodGet, "/v1/runs/mine", "")
	var run Run
	_ = json.Unmarshal([]byte(body), &run)
	if run.State != StateFailed || run.Reason != "DeadlineExceeded" || run.ExitCode == nil || *run.ExitCode != 3 {
		t.Fatalf("run = %+v", run)
	}

	theirs := h.cluster.pods["tenant-other-runs/"+PodName(Hash("other", "flow", "theirs"), 1)]
	h.cluster.end(theirs, corev1.PodSucceeded, 0, now.Add(-9*time.Minute))
	swept, err := Sweep(context.Background(), h.cluster, twoTenants(), 10*time.Minute, now)
	if err != nil || swept != 1 || len(h.cluster.pods) != 2 {
		t.Fatalf("swept %d, err %v, %d pods left", swept, err, len(h.cluster.pods))
	}
	if status, _ := h.call(flowKey, http.MethodGet, "/v1/runs/mine", ""); status != http.StatusNotFound {
		t.Fatalf("a swept run: %d", status)
	}

	if status, _ := h.call(otherKey, http.MethodDelete, "/v1/runs/mine", ""); status != http.StatusNoContent {
		t.Fatalf("cancel: %d", status)
	}
	if len(h.cluster.pods) != 1 {
		t.Fatalf("pods after the cancel = %d", len(h.cluster.pods))
	}
}

// Before a table is read, and for a tenant the table does not name, nothing
// is started.
func TestWithoutATableEveryKeyIsRefused(t *testing.T) {
	srv := httptest.NewServer(NewServer(&cluster{pods: map[string]*corev1.Pod{}}, program, slog.New(slog.NewJSONHandler(io.Discard, nil))).Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/actions", nil)
	req.Header.Set("Authorization", "Bearer "+flowKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp, err := http.Get(srv.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("healthz: %v %v", resp, err)
	}
}
