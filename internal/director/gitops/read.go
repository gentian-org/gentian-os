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

package gitops

import (
	"context"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// App is one entry of a tenant's apps list as git records it.
type App struct {
	Profile string   `json:"profile"`
	Addons  []string `json:"addons,omitempty"`
}

// Apps returns what git says a tenant has installed. Git is the desired state;
// what the cluster has made of it is the operator's to report, not the
// director's.
func (g *GitOps) Apps(ctx context.Context, tenant string) ([]App, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFile(ctx, tenant)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Apps []App `json:"apps"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if doc.Spec.Apps == nil {
		return []App{}, nil
	}
	return doc.Spec.Apps, nil
}
