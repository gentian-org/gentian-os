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

package applifecycle

import "testing"

func TestSetAddonsRequestProvisionsPerAddon(t *testing.T) {
	t.Parallel()
	// The store offers Install and Provision per row, so a single save carries
	// both answers. Collapsing them is how an installed CRM ended up with a group
	// carrying sales_team.group_sale_salesman and no members in it.
	req := SetAddonsRequest{
		Addons:       []string{"odoo-crm-ce", "odoo-contacts-ce"},
		ProvisionFor: []string{"odoo-contacts-ce"},
	}
	if req.provisions("odoo-crm-ce") {
		t.Fatal("odoo-crm-ce was installed, not provisioned — it must not be granted")
	}
	if !req.provisions("odoo-contacts-ce") {
		t.Fatal("odoo-contacts-ce was provisioned and must be granted")
	}
}

func TestSetAddonsRequestProvisionAllStillMeansAll(t *testing.T) {
	t.Parallel()
	// The old wire field, kept for callers that mean "all of them".
	req := SetAddonsRequest{
		Addons:    []string{"odoo-crm-ce", "odoo-contacts-ce"},
		Provision: true,
	}
	for _, name := range req.Addons {
		if !req.provisions(name) {
			t.Fatalf("Provision:true must grant every addon, missed %s", name)
		}
	}
}

func TestSetAddonsRequestProvisionForWinsOverProvision(t *testing.T) {
	t.Parallel()
	// A caller that sends both means the finer answer. Without this, the store's
	// "did you provision anything at all" bool would re-widen the selection to
	// every addon in the save — the exact behaviour being fixed.
	req := SetAddonsRequest{
		Addons:       []string{"odoo-crm-ce", "odoo-contacts-ce"},
		Provision:    true,
		ProvisionFor: []string{"odoo-contacts-ce"},
	}
	if req.provisions("odoo-crm-ce") {
		t.Fatal("ProvisionFor must decide when given, and it omits odoo-crm-ce")
	}
	if !req.provisions("odoo-contacts-ce") {
		t.Fatal("odoo-contacts-ce is named in ProvisionFor and must be granted")
	}
}

func TestSetAddonsRequestGrantsNothingByDefault(t *testing.T) {
	t.Parallel()
	// Installing without asking to provision leaves access to group assignment,
	// which is the documented behaviour and must stay that way.
	req := SetAddonsRequest{Addons: []string{"odoo-crm-ce"}}
	if req.provisions("odoo-crm-ce") {
		t.Fatal("no provisioning was requested, so nothing may be granted")
	}
}
