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

package authz

import (
	"context"
	"fmt"
	"sort"
)

// ClusterRoles are the cluster's role relations, each named by the Keycloak
// group that holds it. The names are the model's relations on type cluster;
// anything else is refused rather than written, because a tuple naming a
// relation the model does not have is accepted by OpenFGA and then matches
// nothing, which is indistinguishable from a permission that was never granted.
var ClusterRoles = []string{
	"admin",
	"security_officer",
	"auditor",
	"service_admin",
	"shared_apps_admin",
	"break_glass",
}

func knownClusterRole(rel string) bool {
	for _, r := range ClusterRoles {
		if r == rel {
			return true
		}
	}
	return false
}

// ReconcileClusterRoles makes the cluster's role tuples equal to what the
// Cluster claim assigns: for each relation, the members of one Keycloak group
// hold it.
//
// Declarative, not additive. A role whose group the claim drops loses its
// tuple, so authority over a cluster can be taken away by editing the claim
// rather than by remembering to delete something. That is also why this reads
// what is stored first: the claim is the whole truth about these relations,
// and anything else found under them was either a previous claim's answer or
// something nobody declared.
//
// Group MEMBERSHIP is not touched here. It is a projection of Keycloak, fed by
// the event listener, and the two must not both write it.
func (c *OpenFGA) ReconcileClusterRoles(ctx context.Context, cluster string, roles map[string]string) error {
	object := Cluster(cluster)

	// A key the model has no relation for is a mistake in the claim, and a
	// silent one: OpenFGA accepts the tuple and it then matches nothing.
	for rel := range roles {
		if !knownClusterRole(rel) {
			return fmt.Errorf("cluster role %q is not a relation of type cluster", rel)
		}
	}

	want := map[string]Tuple{}
	for _, rel := range ClusterRoles {
		group, ok := roles[rel]
		if !ok || group == "" {
			continue
		}
		obj, err := Group(group)
		if err != nil {
			return fmt.Errorf("role %s names group %q: %w", rel, group, err)
		}
		t := Tuple{User: obj + "#member", Relation: rel, Object: object}
		want[key(t)] = t
	}

	var writes, deletes []Tuple
	for _, rel := range ClusterRoles {
		have, err := c.Read(ctx, Tuple{Relation: rel, Object: object})
		if err != nil {
			return fmt.Errorf("read %s on %s: %w", rel, object, err)
		}
		for _, t := range have {
			if _, keep := want[key(t)]; keep {
				delete(want, key(t))
				continue
			}
			deletes = append(deletes, Tuple{User: t.User, Relation: t.Relation, Object: t.Object})
		}
	}
	for _, t := range want {
		writes = append(writes, t)
	}
	if len(writes) == 0 && len(deletes) == 0 {
		return nil
	}
	// Sorted so a log line or a test reads the same way twice.
	sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
	sort.Slice(deletes, func(i, j int) bool { return key(deletes[i]) < key(deletes[j]) })

	if err := c.Write(ctx, writes, deletes); err != nil {
		return fmt.Errorf("reconcile cluster roles: %w", err)
	}
	c.log.InfoContext(ctx, "cluster roles reconciled",
		"cluster", cluster, "granted", len(writes), "revoked", len(deletes))
	return nil
}

func key(t Tuple) string { return t.User + "|" + t.Relation + "|" + t.Object }
