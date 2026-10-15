/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package perimetertraffic

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/usage"
)

// The sampler lists the Envoy pods of the edge and the perimeter routes, and
// reads each pod's metrics port. It reads no Secret of the edge and writes
// nothing there.
//
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch

// Envoy Gateway's labels on the proxies it runs, and the port they serve
// their counters on.
var proxyLabels = client.MatchingLabels{
	"app.kubernetes.io/managed-by": "envoy-gateway",
	"app.kubernetes.io/component":  "proxy",
}

const (
	metricsPortName    = "metrics"
	defaultMetricsPort = 19001
	// The publishing proxy's own label on everything it is made of, the
	// HTTPRoute included (controller/component_perimeter.go).
	perimeterAppName = "perimeter-proxy"
	componentLabel   = "gentianos.io/component"
	exposureLabel    = "gentianos.io/exposure"
	// How long one reading of one pod may take.
	scrapeTimeout = 10 * time.Second
	// How often old readings are pruned, at most.
	pruneEvery = 24 * time.Hour
)

// Sampler reads the perimeter's counters on a ticker and records, for each
// tenant, what each of its public paths carried since the reading before.
type Sampler struct {
	// Client reads uncached: the edge's pods are listed a few times an hour,
	// and caching every pod of the cluster for that is not worth its memory.
	Client client.Reader
	// Interval is how often the counters are read.
	Interval time.Duration
	// Retention is how long readings are kept. Zero keeps them.
	Retention time.Duration
	// Scrape reads one Envoy pod's counters. Nil reads its metrics port
	// over HTTP; tests put the exposition in directly.
	Scrape func(ctx context.Context, pod *corev1.Pod) ([]byte, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu        sync.Mutex
	last      map[types.UID]map[Rule]Counts
	lastPass  time.Time
	lastPrune time.Time
}

// Start runs until ctx is cancelled. It implements manager.Runnable.
func (s *Sampler) Start(ctx context.Context) error {
	s.RunOnce(ctx)
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// NeedLeaderElection keeps one replica reading: two would record every
// request twice.
func (s *Sampler) NeedLeaderElection() bool { return true }

func (s *Sampler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// RunOnce reads every Envoy of the edge once and records the difference.
//
// The first reading of a pod is its baseline and records nothing, unless
// the pod started after the pass before: then everything it counted is new.
// A counter that went down was reset, and what it reads is new. So the
// operator restarting loses at most one interval, and an Envoy restarting
// loses nothing it counted after the previous pass.
func (s *Sampler) RunOnce(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("perimeter-traffic")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[types.UID]map[Rule]Counts{}
	}
	observed := s.now()

	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods, client.InNamespace(layout.Namespace(layout.Edge)), proxyLabels); err != nil {
		logger.Error(err, "failed to list the edge's proxies")
		return
	}
	delta := map[Rule]Counts{}
	seen := map[types.UID]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		body, err := s.scrape(ctx, pod)
		if err != nil {
			logger.Error(err, "failed to read a proxy's counters", "pod", pod.Name)
			continue
		}
		current := Parse(body)
		seen[pod.UID] = true
		before, known := s.last[pod.UID]
		s.last[pod.UID] = current
		if !known {
			started := pod.Status.StartTime
			if s.lastPass.IsZero() || started == nil || started.Time.Before(s.lastPass) {
				continue // a baseline: what it counted before is not this interval's
			}
		}
		for rule, counts := range current {
			d := counts.minus(before[rule])
			total := delta[rule]
			total.add(d)
			delta[rule] = total
		}
	}
	for uid := range s.last {
		if !seen[uid] {
			delete(s.last, uid)
		}
	}
	s.lastPass = observed

	byTenant, err := s.rows(ctx, delta, observed)
	if err != nil {
		logger.Error(err, "failed to read the perimeter routes")
		return
	}
	prune := s.Retention > 0 && observed.Sub(s.lastPrune) >= pruneEvery
	for tenant, rows := range byTenant {
		store, err := storeFor(ctx, s.Client, tenant)
		if err != nil {
			logger.Error(err, "no database to record traffic in", "tenant", tenant)
			continue
		}
		if err := store.Record(ctx, rows); err != nil {
			logger.Error(err, "failed to record traffic", "tenant", tenant)
			continue
		}
		if prune {
			if err := store.Prune(ctx, observed.Add(-s.Retention)); err != nil {
				logger.Error(err, "failed to prune traffic", "tenant", tenant)
			}
		}
	}
	if prune {
		s.lastPrune = observed
	}
}

// rows names each rule's traffic by tenant, entry and path. A rule whose
// route is gone -- the entry was withdrawn between two readings -- is kept
// under the route's name, so no request it carried goes uncounted.
func (s *Sampler) rows(ctx context.Context, delta map[Rule]Counts, observed time.Time) (map[string][]Row, error) {
	var routes gatewayv1.HTTPRouteList
	if err := s.Client.List(ctx, &routes, client.MatchingLabels{"app.kubernetes.io/name": perimeterAppName}); err != nil {
		return nil, err
	}
	known := map[types.NamespacedName]*gatewayv1.HTTPRoute{}
	for i := range routes.Items {
		route := &routes.Items[i]
		known[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}] = route
	}
	out := map[string][]Row{}
	for rule, counts := range delta {
		if counts.zero() {
			continue
		}
		tenant, ok := tenantOfDMZ(rule.Namespace)
		if !ok {
			continue
		}
		row := Row{ObservedAt: observed, Exposure: rule.Route, Path: "#" + strconv.Itoa(rule.Index), Counts: counts}
		if route := known[types.NamespacedName{Namespace: rule.Namespace, Name: rule.Route}]; route != nil {
			row.Component = route.Labels[componentLabel]
			row.Exposure = route.Labels[exposureLabel]
			if rule.Index < len(route.Spec.Rules) {
				if path := rulePath(route.Spec.Rules[rule.Index]); path != "" {
					row.Path = path
				}
			}
		}
		out[tenant] = append(out[tenant], row)
	}
	return out, nil
}

func rulePath(rule gatewayv1.HTTPRouteRule) string {
	for _, m := range rule.Matches {
		if m.Path != nil && m.Path.Value != nil {
			return *m.Path.Value
		}
	}
	return ""
}

// tenantOfDMZ is the tenant whose DMZ a namespace is.
func tenantOfDMZ(namespace string) (string, bool) {
	name, ok := strings.CutPrefix(namespace, "tenant-")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, "-dmz")
	if !ok || name == "" || layout.TenantDMZ(name) != namespace {
		return "", false
	}
	return name, true
}

func (s *Sampler) scrape(ctx context.Context, pod *corev1.Pod) ([]byte, error) {
	if s.Scrape != nil {
		return s.Scrape(ctx, pod)
	}
	port := defaultMetricsPort
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == metricsPortName {
				port = int(p.ContainerPort)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	// Only the counters of route rules: the exposition is otherwise a few
	// megabytes, nearly all of it about listeners and the kernel's routes.
	address := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port)) +
		"/stats/prometheus?usedonly&filter=" + "httproute/tenant-.*-dmz/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", pod.Name, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// storeFor is a tenant's traffic store, in its own database. A variable, so
// a test can keep readings in memory.
var storeFor = func(ctx context.Context, c client.Reader, tenant string) (recorder, error) {
	s, err := usage.StoreForTenant(ctx, c, layout.Tenant(tenant), tenant)
	if err != nil {
		return nil, err
	}
	return NewStore(s), nil
}

type recorder interface {
	Record(ctx context.Context, rows []Row) error
	Prune(ctx context.Context, before time.Time) error
}
