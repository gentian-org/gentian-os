/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.roles;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

class RoleNamesTest {

    private static final Map<String, Map<String, List<String>>> IN_APP = Map.of(
        "odoo-base-ce", Map.of("user", List.of("base.group_user"), "admin", List.of("base.group_system")),
        "odoo-sales-ce", Map.of(
            "user", List.of("sales_team.group_sale_salesman"),
            "admin", List.of("sales_team.group_sale_manager")));

    @Test
    void aRoleHeldIsWrittenAsTheAppCallsIt() {
        Map<String, List<String>> held = Map.of(
            "odoo-base-ce", List.of("user"),
            "odoo-sales-ce", List.of("admin", "user"));
        assertEquals(
            List.of("base.group_user", "sales_team.group_sale_manager", "sales_team.group_sale_salesman"),
            RoleNames.held(IN_APP, held));
    }

    @Test
    void aRoleTheAppHasNoNameForAndAnAppThePlatformDidNotNameSayNothing() {
        Map<String, List<String>> held = Map.of(
            "odoo-base-ce", List.of("user", "auditor"),
            "odoo-crm-ce", List.of("admin"));
        assertEquals(List.of("base.group_user"), RoleNames.held(IN_APP, held));
    }

    @Test
    void aPersonWhoHoldsNothingGetsAnEmptyListNotAnAbsentClaim() {
        assertEquals(List.of(), RoleNames.held(IN_APP, Map.of()));
    }

    @Test
    void everyNameThePlatformDecidesIsListedOnce() {
        assertEquals(
            List.of("base.group_system", "base.group_user",
                "sales_team.group_sale_manager", "sales_team.group_sale_salesman"),
            RoleNames.mapped(IN_APP));
    }

    @Test
    void onlyASignInFailsWhenTheStoreCannotBeAsked() {
        assertFalse(RoleNames.mayKeepPrevious("authorization_code"));
        assertTrue(RoleNames.mayKeepPrevious("refresh_token"));
        assertTrue(RoleNames.mayKeepPrevious(null));
    }

    @Test
    void theAnswerIsReadByProfileName() throws IOException {
        Map<String, List<String>> held = RolesClient.parse(
            "{\"apps\":[{\"app\":\"odoo-base-ce\",\"roles\":[\"user\"]},{\"app\":\"odoo-sales-ce\",\"roles\":[\"admin\",\"user\"]}]}");
        assertEquals(List.of("user"), held.get("odoo-base-ce"));
        assertEquals(List.of("admin", "user"), held.get("odoo-sales-ce"));
        assertEquals(Map.of(), RolesClient.parse("{\"apps\":[]}"));
    }

    @Test
    void anAnswerThatNamesNoAppsIsNotAnAnswer() {
        assertThrows(IOException.class, () -> RolesClient.parse("{}"));
    }

    @Test
    void theQuestionNamesTheTenantTheAppAndThePerson() throws IOException {
        assertEquals("{\"tenant\":\"acme\",\"app\":\"odoo-base-ce\",\"person\":\"p-1\"}",
            RolesClient.question("acme", "odoo-base-ce", "p-1"));
    }

    @Test
    void theQuestionAsksForClusterAndTenantRolesTheSameWay() throws IOException {
        assertEquals("{\"cluster\":true,\"realm\":\"kernel\",\"person\":\"p-1\"}",
            RolesClient.question(null, null, "kernel", "p-1"));
        assertEquals("{\"tenant\":\"acme\",\"person\":\"p-1\"}",
            RolesClient.question("acme", null, null, "p-1"));
        RolesClient.Answer answer = RolesClient.parseAnswer(
            "{\"apps\":[],\"cluster\":[\"admin\",\"auditor\"],\"tenant\":[\"user\"]}");
        assertEquals(List.of("admin", "auditor"), answer.cluster);
        assertEquals(List.of("user"), answer.tenant);
    }

    @Test
    void aPartnerAssertionNamesWhoAdministers() {
        assertEquals(List.of("cluster-admin", "tenant-admin"),
            PartnerRolesMapper.roles(List.of("admin", "user"), List.of("admin")));
        assertEquals(List.of("tenant-admin"), PartnerRolesMapper.roles(List.of("admin", "user"), List.of("auditor")));
        assertEquals(List.of(), PartnerRolesMapper.roles(List.of("user"), List.of()));
    }
}
