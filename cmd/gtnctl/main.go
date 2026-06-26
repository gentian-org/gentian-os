/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing limitations and the License.
*/

// gtnctl is the Gentian OS CLI for tenant app lifecycle (install/uninstall/purge).
// kubectl-gentian delegates apps install/uninstall to this binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gentian-org/gentian-os/internal/applifecycle"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(gentianov1alpha1.AddToScheme(scheme))
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "apps":
		if err := runApps(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `gtnctl — Gentian OS app lifecycle CLI

Usage:
  gtnctl apps install <profile> --tenant <tenant>
  gtnctl apps uninstall <profile> --tenant <tenant> [--purge|-f]

Environment:
  GENTIAN_DEPLOYMENTS_PATH     local checkout of gentian-deployments (required)
  GENTIAN_DEPLOYMENTS_REPO     used when deployments path needs cloning
  GENTIAN_DEPLOYMENTS_CLUSTER  cluster selector (default: default-cluster)
  GENTIAN_DEPLOYMENTS_STAGE    stage selector (default: dev)
  KERNEL_NAMESPACE             default platform-kernel
`)
}

func runApps(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing apps subcommand")
	}
	sub := args[0]
	restArgs := args[1:]
	switch sub {
	case "install", "uninstall":
		return runAppMutation(sub, restArgs)
	default:
		return fmt.Errorf("unknown apps subcommand %q", sub)
	}
}

func runAppMutation(action string, args []string) error {
	fs := flag.NewFlagSet("apps "+action, flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant name")
	purge := fs.Bool("purge", false, "purge persistent state (uninstall only)")
	force := fs.Bool("f", false, "alias for --purge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	profile := fs.Arg(0)
	if profile == "" || *tenant == "" {
		return fmt.Errorf("usage: gtnctl apps %s <profile> --tenant <tenant>", action)
	}
	doPurge := *purge || *force
	if action == "install" && doPurge {
		return fmt.Errorf("--purge is only supported for uninstall")
	}
	if os.Getenv("GENTIAN_DEPLOYMENTS_PATH") == "" {
		return fmt.Errorf("GENTIAN_DEPLOYMENTS_PATH is not set")
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return err
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	svc, err := applifecycle.NewService(c, cfg, serviceOpts())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	actor := os.Getenv("USER")
	if actor == "" {
		actor = "gtnctl"
	}

	switch action {
	case "install":
		res, err := svc.Install(ctx, applifecycle.InstallRequest{
			Tenant:  *tenant,
			Profile: profile,
			Actor:   actor,
			Wait:    true,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Done. %s on tenant %s (%s, ready=%v)\n", res.Profile, res.Tenant, res.Status, res.Ready)
	case "uninstall":
		res, err := svc.Uninstall(ctx, applifecycle.UninstallRequest{
			Tenant:  *tenant,
			Profile: profile,
			Purge:   doPurge,
			Actor:   actor,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Done. %s on tenant %s (%s", res.Profile, res.Tenant, res.Status)
		if res.Purged {
			fmt.Print(", purged")
		}
		fmt.Println(")")
		for _, w := range res.Warnings {
			fmt.Printf("WARNING: %s\n", w)
		}
	}
	return nil
}

func serviceOpts() applifecycle.Options {
	return applifecycle.Options{
		KernelNamespace:    envOr("KERNEL_NAMESPACE", "platform-kernel"),
		OpenBaoNamespace:   envOr("OPENBAO_NAMESPACE", "openbao"),
		OperatorNamespace:  envOr("OPERATOR_NAMESPACE", "gentian-system"),
		OperatorSA:         envOr("OPERATOR_SA", "gentian-os"),
		DeploymentsPath:    os.Getenv("GENTIAN_DEPLOYMENTS_PATH"),
		DeploymentsRepo:    os.Getenv("GENTIAN_DEPLOYMENTS_REPO"),
		DeploymentsCluster: envOr("GENTIAN_DEPLOYMENTS_CLUSTER", "default-cluster"),
		DeploymentsStage:   envOr("GENTIAN_DEPLOYMENTS_STAGE", "dev"),
		WaitTimeout:        15 * time.Minute,
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
