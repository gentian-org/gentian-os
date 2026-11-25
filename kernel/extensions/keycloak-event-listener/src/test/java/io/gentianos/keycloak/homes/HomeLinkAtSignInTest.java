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
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import java.util.Map;

import org.junit.jupiter.api.Test;

class HomeLinkAtSignInTest {

    @Test
    void aClientLinksToTheEntriesItNames() {
        assertEquals(List.of("vouch-odoo-base-ce", "vouch-homesync"),
            HomeLinkAtSignIn.aliasesFor(Map.of("gentianos.io/link-to", "vouch-odoo-base-ce, vouch-homesync,vouch-homesync")));
    }

    @Test
    void aClientThatNamesNoEntryLinksNowhere() {
        assertTrue(HomeLinkAtSignIn.aliasesFor(null).isEmpty());
        assertTrue(HomeLinkAtSignIn.aliasesFor(Map.of()).isEmpty());
        assertTrue(HomeLinkAtSignIn.aliasesFor(Map.of("gentianos.io/link-to", " ,vouch-")).isEmpty());
        // Only vouching entries: a client cannot have people linked to any
        // identity provider of the realm.
        assertTrue(HomeLinkAtSignIn.aliasesFor(Map.of("gentianos.io/link-to", "google,partner-x")).isEmpty());
    }

    @Test
    void onlyAnEntryThatSaysSoLinksAtSignIn() {
        assertTrue(HomeLinkAtSignIn.linksAtSignIn(Map.of("gentianos.io/link-at-sign-in", "true")));
        assertFalse(HomeLinkAtSignIn.linksAtSignIn(Map.of("gentianos.io/link-at-sign-in", "false")));
        assertFalse(HomeLinkAtSignIn.linksAtSignIn(Map.of("jwtAuthorizationGrantEnabled", "true")));
        assertFalse(HomeLinkAtSignIn.linksAtSignIn(null));
    }
}
