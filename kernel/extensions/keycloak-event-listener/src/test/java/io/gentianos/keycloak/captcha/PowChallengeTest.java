/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.captcha;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.Test;

/** The arithmetic the browser and the server have to agree on. */
class PowChallengeTest {

    /** Finds a nonce the way the theme's script does: counting up from zero. */
    private static String solve(String salt, int bits) {
        for (long n = 0; ; n++) {
            String nonce = Long.toString(n);
            if (PowChallenge.solves(salt, nonce, bits)) {
                return nonce;
            }
        }
    }

    @Test
    void aSolutionFoundTheBrowsersWayIsAccepted() {
        String salt = PowChallenge.newSalt();
        String nonce = solve(salt, 12);
        assertTrue(PowChallenge.solves(salt, nonce, 12));
        // The same digest has the bits a stricter challenge wants only by luck.
        assertFalse(PowChallenge.solves(salt, nonce, 60));
    }

    @Test
    void anotherSaltsSolutionIsWorthNothing() {
        String salt = PowChallenge.newSalt();
        String nonce = solve(salt, 12);
        assertFalse(PowChallenge.solves(PowChallenge.newSalt(), nonce, 12));
    }

    @Test
    void whatIsNotANonceSolvesNothing() {
        String salt = PowChallenge.newSalt();
        assertFalse(PowChallenge.solves(salt, null, 1));
        assertFalse(PowChallenge.solves(salt, "", 1));
        assertFalse(PowChallenge.solves(salt, "12a", 1));
        assertFalse(PowChallenge.solves(salt, "1".repeat(101), 1));
        assertFalse(PowChallenge.solves(null, "1", 1));
    }

    @Test
    void leadingZeroBitsCountsAcrossBytes() {
        assertEquals(0, PowChallenge.leadingZeroBits(new byte[] {(byte) 0x80}));
        assertEquals(7, PowChallenge.leadingZeroBits(new byte[] {0x01}));
        assertEquals(8, PowChallenge.leadingZeroBits(new byte[] {0x00, (byte) 0xff}));
        assertEquals(13, PowChallenge.leadingZeroBits(new byte[] {0x00, 0x04}));
        assertEquals(16, PowChallenge.leadingZeroBits(new byte[] {0x00, 0x00}));
    }

    @Test
    void saltsDiffer() {
        assertNotEquals(PowChallenge.newSalt(), PowChallenge.newSalt());
        assertEquals(32, PowChallenge.newSalt().length());
    }
}
