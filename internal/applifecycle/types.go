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

package applifecycle

import "time"

// Options configures the lifecycle service.
type Options struct {
	KernelNamespace     string
	OpenBaoNamespace    string
	OperatorNamespace   string
	OperatorSA          string
	DeploymentsPath     string
	DeploymentsRepo     string
	DeploymentsCluster  string
	DeploymentsStage    string
	WaitTimeout         time.Duration
}

// InstallRequest installs an app profile on a tenant.
type InstallRequest struct {
	Tenant  string
	Profile string
	Actor   string
}

// UninstallRequest removes an app profile from a tenant.
type UninstallRequest struct {
	Tenant  string
	Profile string
	Purge   bool
	Actor   string
}

// Result is returned from install/uninstall operations.
type Result struct {
	Status   string   `json:"status"`
	Tenant   string   `json:"tenant"`
	Profile  string   `json:"profile"`
	Purged   bool     `json:"purged,omitempty"`
	Ready    bool     `json:"ready,omitempty"`
	Message  string   `json:"message,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}
