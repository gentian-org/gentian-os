/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package controller contains the reconciliation controllers for the Gentian OS operator.
//
// Implemented: Tenant validating webhook (optional via Helm values).
// Deferred: AppProfile validating webhook — tracked on roadmap; catalogue
// integrity is enforced via CRD OpenAPI and gentian-apps CI today.
package controller
