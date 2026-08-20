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
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// AppExecer runs a command inside an app's pod. An interface so tests can
// record which hooks a resume actually ran — the difference between "resumed"
// and "still serving its maintenance page" is exactly one of these calls.
type AppExecer interface {
	Exec(ctx context.Context, namespace, pod, container string, argv []string) (string, error)
}

// PodExecer runs a command inside a running container.
//
// This is what lets a profile's maintenance-mode hooks and its restore hooks
// actually run. Without it `quiesce.mode: command` can only fall back to
// scaling the app down, which pauses writes just as well but takes the app
// offline, and `restore.post` cannot run at all — leaving an app serving
// restored data it has not been told to re-read.
type PodExecer struct {
	Config    *rest.Config
	Clientset kubernetes.Interface
}

// NewPodExecer builds an execer from the manager's own REST config.
func NewPodExecer(cfg *rest.Config) (*PodExecer, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no REST config")
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset for exec: %w", err)
	}
	return &PodExecer{Config: cfg, Clientset: clientset}, nil
}

// Exec runs argv in a container of a running pod and returns its combined
// output. argv is passed through verbatim — no shell — so nothing in a
// profile's hook is word-split or glob-expanded on the way.
func (e *PodExecer) Exec(ctx context.Context, namespace, pod, container string, argv []string) (string, error) {
	if e == nil || e.Clientset == nil {
		return "", fmt.Errorf("exec is not configured")
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}

	req := e.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.Config, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("build executor: %w", err)
	}

	var out, errOut bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &errOut})
	combined := strings.TrimSpace(out.String() + "\n" + errOut.String())
	if err != nil {
		return combined, fmt.Errorf("exec %v in %s/%s: %w", argv, namespace, pod, err)
	}
	return combined, nil
}

// runningPodForApp finds a pod of an installed app to exec into.
//
// It requires a Running pod: a hook run against a terminating or pending pod
// either fails or, worse, succeeds against the wrong instance. When the hook
// names a container, only pods that have that container qualify — the name
// match on workloads is loose enough to also catch an app's sidecar releases,
// and the first live post-restore hook was exec'd into the MCP sidecar's pod
// because the real pod was briefly not Ready. A Running-but-not-Ready pod
// with the right container is a better target than any other pod: after a
// restore the app is often unready precisely because the hook has not run.
func (r *TenantReconciler) runningPodForApp(ctx context.Context, tenantName, appName, container string) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(backup.TenantNamespace(tenantName))); err != nil {
		return nil, err
	}
	var fallback *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		if !workloadBelongsToApp(pod.Labels, pod.Name, appName) {
			continue
		}
		if container != "" && !podHasContainer(pod, container) {
			continue
		}
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			return pod, nil
		}
		if fallback == nil {
			fallback = pod
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("no running pod for app %q in tenant %q", appName, tenantName)
}

func podHasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// execAppCommand runs one of a profile's hooks in the app's own pod.
func (r *TenantReconciler) execAppCommand(
	ctx context.Context,
	tenantName, appName string,
	spec *gentianov1alpha1.BackupSpec,
	argv []string,
) (string, error) {
	if r.Exec == nil {
		return "", fmt.Errorf("exec is not configured on this operator")
	}
	container := spec.QuiesceContainer()
	pod, err := r.runningPodForApp(ctx, tenantName, appName, container)
	if err != nil {
		return "", err
	}
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	return r.Exec.Exec(ctx, pod.Namespace, pod.Name, container, argv)
}
