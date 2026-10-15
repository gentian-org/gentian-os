/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// An orphaned tenant: a Tenant on the cluster that git no longer describes
// and that no Argo CD Application owns.
//
// Argo CD makes every Tenant from the deployments repository, through one
// Application per tenant, and prunes the Tenant when the tenant's directory
// is removed. A sync that was still running when the directory went can
// finish after the Application has been deleted and re-create the Tenant,
// which then has an Argo CD tracking annotation naming an Application that
// does not exist. Nothing reconciles it away: the director answers "tenant
// not found", because git has none, and the operator runs it as any other.
//
// RemoveOrphanTenant deletes exactly that object, and no other: a Tenant
// with no tracking annotation was made by someone else and is theirs to
// remove; one whose Application exists is Argo CD's, and goes when git says
// so. The operator's finalizer then tears the tenant down under the
// deletionPolicy the object carries.

// ArgoTrackingAnnotation is what Argo CD puts on every object it applies:
// "<application>:<group>/<kind>:<namespace>/<name>".
const ArgoTrackingAnnotation = "argocd.argoproj.io/tracking-id"

// ErrNotOrphan is a Tenant this removal is not for: made without Argo CD,
// or the platform's.
var ErrNotOrphan = errors.New("not an orphaned tenant")

// ErrTenantDeclared is a Tenant whose Application still exists: Argo CD's,
// removed through git.
var ErrTenantDeclared = errors.New("the tenant's Application still exists")

// OrphanRemoval says what was removed.
type OrphanRemoval struct {
	Name string `json:"name"`
	// Application is the Argo CD Application the Tenant named, which no
	// longer exists.
	Application string `json:"application"`
	// AlreadyDeleting is true when the Tenant was already on its way out.
	AlreadyDeleting bool `json:"alreadyDeleting,omitempty"`
}

// RemoveOrphanTenant deletes a Tenant that git no longer describes and no
// Application owns. A Tenant the cluster does not hold is a NotFound error.
func (s *Service) RemoveOrphanTenant(ctx context.Context, name, actor string) (*OrphanRemoval, error) {
	tenant := &gentianov1alpha1.Tenant{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: name}, tenant); err != nil {
		return nil, err
	}
	if name == gentianov1alpha1.PlatformTenantName {
		return nil, fmt.Errorf("%w: %s is the platform tenant", ErrNotOrphan, name)
	}
	tracking := tenant.Annotations[ArgoTrackingAnnotation]
	app, _, found := strings.Cut(tracking, ":")
	if !found || app == "" {
		return nil, fmt.Errorf("%w: %s carries no Argo CD tracking annotation, so it was not made from the deployments repository; whoever made it removes it", ErrNotOrphan, name)
	}
	application := &unstructured.Unstructured{}
	application.SetGroupVersionKind(applicationGVK)
	err := s.live().Get(ctx, types.NamespacedName{Namespace: layout.Namespace(layout.GitOps), Name: app}, application)
	switch {
	case err == nil:
		return nil, fmt.Errorf("%w: Application %s owns %s; retire it through git", ErrTenantDeclared, app, name)
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("look for Application %s: %w", app, err)
	}
	res := &OrphanRemoval{Name: name, Application: app}
	if !tenant.DeletionTimestamp.IsZero() {
		res.AlreadyDeleting = true
		return res, nil
	}
	if err := s.client.Delete(ctx, tenant); err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	log.FromContext(ctx).Info("orphaned tenant deleted: no Application owns it",
		"tenant", name, "application", app, "actor", actor)
	return res, nil
}
