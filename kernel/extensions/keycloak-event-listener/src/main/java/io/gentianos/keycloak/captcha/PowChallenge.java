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

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.security.SecureRandom;
import java.util.HexFormat;

/**
 * A proof-of-work challenge: the browser has to find a nonce whose SHA-256,
 * together with the challenge's salt, begins with a number of zero bits. The
 * work is a second or two in a browser and the same for every sign-up, which
 * is nothing to a person and a cost to whoever signs up by the thousand.
 *
 * <p>Nothing but arithmetic: no third party is asked, nobody's browser is
 * profiled, and no key is involved. The challenge is bound to the
 * registration it was made for by the authentication session that carries
 * it, so a solution serves one sign-up and is worth nothing to another.
 */
public final class PowChallenge {

    /** Leading zero bits the digest must have. 2^18 hashes on average. */
    public static final int DEFAULT_BITS = 18;

    /** How long a challenge is good for: the time a person takes over the form, with room. */
    public static final long TTL_SECONDS = 15 * 60;

    private static final SecureRandom RANDOM = new SecureRandom();

    private PowChallenge() {
    }

    /** A fresh salt: 16 random bytes as hex. */
    public static String newSalt() {
        byte[] salt = new byte[16];
        RANDOM.nextBytes(salt);
        return HexFormat.of().formatHex(salt);
    }

    /**
     * Whether {@code nonce} solves the challenge: the digest of
     * {@code salt + ":" + nonce} begins with {@code bits} zero bits. A nonce
     * that is empty, over a hundred characters or not decimal digits solves
     * nothing, whatever it hashes to.
     */
    public static boolean solves(String salt, String nonce, int bits) {
        if (salt == null || nonce == null || nonce.isEmpty() || nonce.length() > 100) {
            return false;
        }
        for (int i = 0; i < nonce.length(); i++) {
            char c = nonce.charAt(i);
            if (c < '0' || c > '9') {
                return false;
            }
        }
        byte[] digest = sha256(salt + ":" + nonce);
        return leadingZeroBits(digest) >= bits;
    }

    /** The number of leading zero bits of a digest. */
    static int leadingZeroBits(byte[] digest) {
        int bits = 0;
        for (byte b : digest) {
            if (b == 0) {
                bits += 8;
                continue;
            }
            bits += Integer.numberOfLeadingZeros(b & 0xff) - 24;
            break;
        }
        return bits;
    }

    static byte[] sha256(String s) {
        try {
            return MessageDigest.getInstance("SHA-256").digest(s.getBytes(StandardCharsets.UTF_8));
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 is part of every Java runtime", e);
        }
    }
}
