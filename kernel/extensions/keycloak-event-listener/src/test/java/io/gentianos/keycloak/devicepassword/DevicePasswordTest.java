/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.devicepassword;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.security.SecureRandom;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.OperationType;
import org.keycloak.events.admin.ResourceType;

class DevicePasswordTest {

    private final SecureRandom random = new SecureRandom();

    @Test
    void aPasswordIsFiveGroupsOfFiveFromTheAlphabet() {
        DevicePassword.Issued issued = DevicePassword.generate(random);
        String typed = issued.formatted();
        assertTrue(typed.matches("^([a-z2-9]{5}-){4}[a-z2-9]{5}$"), typed);
        assertTrue(typed.startsWith(issued.ref() + "-"));
        for (char c : "01ilo".toCharArray()) {
            assertFalse(typed.indexOf(c) >= 0, "a character that reads like another: " + typed);
        }
    }

    @Test
    void aPasswordIsReadBackHoweverItWasTyped() {
        DevicePassword.Issued issued = DevicePassword.generate(random);
        for (String typed : List.of(issued.formatted(), issued.formatted().toUpperCase(),
            issued.formatted().replace("-", " "), issued.ref() + issued.secret())) {
            DevicePassword.Presented p = DevicePassword.parse(typed);
            assertNotNull(p, typed);
            assertEquals(issued.ref(), p.ref());
            assertEquals(issued.secret(), p.secret());
        }
    }

    @Test
    void whatCannotBeAPasswordIsNotRead() {
        assertNull(DevicePassword.parse(null));
        assertNull(DevicePassword.parse("hunter2"));
        assertNull(DevicePassword.parse("aaaaa-aaaaa-aaaaa-aaaaa-aaaa1"), "1 is not in the alphabet");
        assertNull(DevicePassword.parse("aaaaa-aaaaa-aaaaa-aaaaa-aaaaa-a"));
    }

    @Test
    void theDigestIsBoundToThePersonAndTheReference() {
        DevicePassword.Issued issued = DevicePassword.generate(random);
        DevicePassword.Presented p = DevicePassword.parse(issued.formatted());
        String kept = DevicePassword.digest("user-1", issued.ref(), issued.secret());
        assertTrue(DevicePassword.matches("user-1", p, kept));
        assertFalse(DevicePassword.matches("user-2", p, kept), "another person's digest");
        DevicePassword.Presented otherRef = new DevicePassword.Presented("zzzzz", p.secret());
        assertFalse(DevicePassword.matches("user-1", otherRef, kept), "the same secret under another reference");
        assertFalse(DevicePassword.matches("user-1", p, null));
    }

    @Test
    void aPasswordUnusedLongerThanTheRealmsIdleLimitIsIdle() {
        long day = 86_400_000L;
        int thirtyDays = 30 * 86_400;
        long now = 100 * day;
        assertFalse(DevicePassword.idle(now - 29 * day, now - 90 * day, thirtyDays, now));
        assertTrue(DevicePassword.idle(now - 31 * day, now - 90 * day, thirtyDays, now));
        assertTrue(DevicePassword.idle(null, now - 31 * day, thirtyDays, now), "never used counts from creation");
        assertFalse(DevicePassword.idle(null, now - day, thirtyDays, now));
        assertFalse(DevicePassword.idle(now - 365 * day, null, 0, now), "no idle limit");
    }

    @Test
    void namesAndWaysAreChecked() {
        assertNull(DevicePassword.invalidName("Anna's iPhone"));
        assertNotNull(DevicePassword.invalidName(" "));
        assertNotNull(DevicePassword.invalidName("a\nb"));
        assertNotNull(DevicePassword.invalidName("x".repeat(65)));
        assertNull(DevicePassword.invalidWays(List.of("calendars", "contacts")));
        assertNotNull(DevicePassword.invalidWays(List.of()));
        assertNotNull(DevicePassword.invalidWays(List.of("Files")));
        assertNotNull(DevicePassword.invalidWays(List.of("files", "files")));
    }

    @Test
    void aPasswordChangeOrASignOutEverywhereEndsThePersonsDevicePasswords() {
        assertEquals("u", DevicePasswordCleanup.endedBy(EventType.UPDATE_PASSWORD, "u", Map.of()));
        assertEquals("u", DevicePasswordCleanup.endedBy(EventType.UPDATE_CREDENTIAL, "u", Map.of("credential_type", "password")));
        assertNull(DevicePasswordCleanup.endedBy(EventType.UPDATE_CREDENTIAL, "u", Map.of("credential_type", "otp")));
        assertNull(DevicePasswordCleanup.endedBy(EventType.LOGOUT, "u", Map.of()), "one sign-out ends one session");
        assertNull(DevicePasswordCleanup.endedBy(EventType.LOGIN, "u", Map.of()));

        assertEquals("u", DevicePasswordCleanup.endedBy(ResourceType.USER, OperationType.ACTION, "users/u/reset-password"));
        assertEquals("u", DevicePasswordCleanup.endedBy(ResourceType.USER, OperationType.ACTION, "users/u/logout"));
        assertNull(DevicePasswordCleanup.endedBy(ResourceType.USER, OperationType.UPDATE, "users/u"));
        assertNull(DevicePasswordCleanup.endedBy(ResourceType.USER, OperationType.ACTION, "users/u/send-verify-email"));
    }
}
