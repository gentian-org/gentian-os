/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// The dispatcher starts runs.
//
// The operator follows git and keeps state; a run is neither. A component
// that declares requires.services.runs asks here, with its key, for one run
// of an action installed in its tenant or of an image of its own, and the
// dispatcher starts one pod for it in the tenant's namespace for runs. Who
// may start what is the operator's table (dispatcher-runs), which this
// program reads and never writes. Its rights at the Kubernetes API are pods
// in those namespaces and, for the director's acts on its second listener
// (internal/dispatcher/acts.go), the record of a backup: it reads no
// profile, no grant and no Secret, and a run's data never passes through it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/dispatcher"
	"github.com/gentian-org/gentian-os/internal/envvar"
	"github.com/gentian-org/gentian-os/internal/tokenreview"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("dispatcher stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// The image the run program is taken from: this program's own, so that
	// what starts a run and what wraps it are one build.
	image := os.Getenv("DISPATCHER_PROGRAM_IMAGE")
	if image == "" {
		return errors.New("DISPATCHER_PROGRAM_IMAGE is required: the image a run's pod takes the run program from")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pods := dispatcher.ClusterPods{Client: kube}
	srv := dispatcher.NewServer(pods, image, log)
	go dispatcher.Follow(ctx, envvar.Or("DISPATCHER_TABLE", "/etc/dispatcher/"+dispatcher.TableKey), 5*time.Second, srv, log)
	// A finished run's pod is kept ten minutes, as the platform's Jobs are.
	go dispatcher.KeepSwept(ctx, pods, srv, 10*time.Minute, time.Minute, log)

	api := &http.Server{
		Addr: envvar.Or("DISPATCHER_LISTEN", ":8080"), Handler: srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second,
	}
	// The director's acts, on a listener of their own. Its caller is known
	// by a token of the director's ServiceAccount issued for this audience;
	// without the two settings the listener refuses every request.
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	records, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	director := ""
	if namespace, account := os.Getenv("POD_NAMESPACE"), os.Getenv("DISPATCHER_DIRECTOR_SERVICE_ACCOUNT"); namespace != "" && account != "" {
		director = "system:serviceaccount:" + namespace + ":" + account
	}
	review := tokenreview.New(kube.AuthenticationV1().TokenReviews(), os.Getenv("DISPATCHER_AUDIENCE"))
	if director == "" || !review.Ready() {
		log.Warn("the listener for the director's acts refuses every request: no audience or no director configured",
			"settings", "DISPATCHER_AUDIENCE, DISPATCHER_DIRECTOR_SERVICE_ACCOUNT, POD_NAMESPACE")
	}
	acts := &http.Server{
		Addr:              envvar.Or("DISPATCHER_ACTS_LISTEN", ":8081"),
		Handler:           dispatcher.NewActs(dispatcher.ClusterExports{Client: records}, review, director, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second,
	}

	done := make(chan error, 2)
	go func() { done <- api.ListenAndServe() }()
	go func() { done <- acts.ListenAndServe() }()
	log.Info("dispatcher listening", "addr", api.Addr, "acts", acts.Addr)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(api.Shutdown(shutdown), acts.Shutdown(shutdown))
}
