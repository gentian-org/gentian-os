/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */

package io.gentianos.keycloak.homes;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.Map;

import org.junit.jupiter.api.Test;

class HomeLinkAtSignInTest {

    @Test
    void aClientThatNamesItsProfileLinksToItsVouchingEntry() {
        assertEquals("vouch-odoo-base-ce", HomeLinkAtSignIn.aliasFor(Map.of("gentianos.io/profile", "odoo-base-ce")));
    }

    @Test
    void aClientThatNamesNoProfileLinksNowhere() {
        assertNull(HomeLinkAtSignIn.aliasFor(null));
        assertNull(HomeLinkAtSignIn.aliasFor(Map.of()));
        assertNull(HomeLinkAtSignIn.aliasFor(Map.of("gentianos.io/profile", " ")));
        assertNull(HomeLinkAtSignIn.aliasFor(Map.of("gentianos.io/managed-by", "platform")));
    }

    @Test
    void onlyAnEntryThatSaysSoLinksAtSignIn() {
        assertTrue(HomeLinkAtSignIn.linksAtSignIn(Map.of("gentianos.io/link-at-sign-in", "true")));
        assertFalse(HomeLinkAtSignIn.linksAtSignIn(Map.of("gentianos.io/link-at-sign-in", "false")));
        assertFalse(HomeLinkAtSignIn.linksAtSignIn(Map.of("jwtAuthorizationGrantEnabled", "true")));
        assertFalse(HomeLinkAtSignIn.linksAtSignIn(null));
    }
}
