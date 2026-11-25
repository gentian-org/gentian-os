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

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.security.SecureRandom;
import java.util.Base64;
import java.util.List;
import java.util.Locale;
import java.util.regex.Pattern;

/**
 * What a device password is, apart from Keycloak: how one is made, read back
 * and checked.
 *
 * <p>A device password is five groups of five characters, such as
 * {@code k3m9x-p2hrt-...}. The first group is its reference: it names the
 * credential among the person's own and is no secret. The other twenty
 * characters are the secret, about 99 bits drawn by {@link SecureRandom}.
 * Only a SHA-256 digest of the secret is kept, bound to the person and the
 * reference. A slow password hash protects a secret a person chose and might
 * be guessed; a random secret of this length is not guessed, and a fast
 * digest keeps the check the door asks for on every new token cheap, as the
 * digests of access tokens at code hosts are.
 *
 * <p>The alphabet leaves out what reads alike (0 o, 1 i l), so the password
 * can be typed from a screen into a phone. Case, blanks and dashes are
 * ignored when it is read back.
 */
public final class DevicePassword {

    /** The credential type a device password is stored as. */
    public static final String TYPE = "gentian-device-password";

    /** How many a person may hold, as at Apple. */
    public static final int MAX_PER_PERSON = 25;

    static final String ALPHABET = "abcdefghjkmnpqrstuvwxyz23456789";
    static final int GROUP = 5;
    static final int GROUPS = 5;

    private static final Pattern WAY = Pattern.compile("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$");
    private static final int MAX_WAYS = 16;
    private static final int MAX_NAME = 64;

    private DevicePassword() {
    }

    /** A password as it is handed out once, with its reference. */
    public record Issued(String ref, String secret) {
        /** The password as the person types it. */
        public String formatted() {
            String all = ref + secret;
            StringBuilder out = new StringBuilder();
            for (int i = 0; i < all.length(); i += GROUP) {
                if (i > 0) {
                    out.append('-');
                }
                out.append(all, i, i + GROUP);
            }
            return out.toString();
        }
    }

    /** A password as it was presented, split into reference and secret. */
    public record Presented(String ref, String secret) {
    }

    /** Draws a new password. */
    public static Issued generate(SecureRandom random) {
        return new Issued(draw(random, GROUP), draw(random, GROUP * (GROUPS - 1)));
    }

    /** Draws a reference alone, when the first one is already the person's. */
    public static String drawRef(SecureRandom random) {
        return draw(random, GROUP);
    }

    private static String draw(SecureRandom random, int n) {
        StringBuilder out = new StringBuilder(n);
        for (int i = 0; i < n; i++) {
            out.append(ALPHABET.charAt(random.nextInt(ALPHABET.length())));
        }
        return out.toString();
    }

    /**
     * Reads a presented password, or null when it cannot be one: the wrong
     * length, or a character the alphabet does not have.
     */
    public static Presented parse(String presented) {
        if (presented == null) {
            return null;
        }
        String s = presented.replaceAll("[\\s-]", "").toLowerCase(Locale.ROOT);
        if (s.length() != GROUP * GROUPS) {
            return null;
        }
        for (int i = 0; i < s.length(); i++) {
            if (ALPHABET.indexOf(s.charAt(i)) < 0) {
                return null;
            }
        }
        return new Presented(s.substring(0, GROUP), s.substring(GROUP));
    }

    /** The digest kept of a secret, bound to its person and reference. */
    public static String digest(String userId, String ref, String secret) {
        try {
            MessageDigest sha = MessageDigest.getInstance("SHA-256");
            byte[] d = sha.digest((userId + "\n" + ref + "\n" + secret).getBytes(StandardCharsets.UTF_8));
            return Base64.getEncoder().encodeToString(d);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 is missing", e);
        }
    }

    /** Whether a secret is the one a digest was kept of, in constant time. */
    public static boolean matches(String userId, Presented p, String kept) {
        if (kept == null) {
            return false;
        }
        byte[] want = digest(userId, p.ref(), p.secret()).getBytes(StandardCharsets.UTF_8);
        return MessageDigest.isEqual(want, kept.getBytes(StandardCharsets.UTF_8));
    }

    /**
     * Whether a password has lain unused longer than the realm lets a
     * long-lived session idle. A password never used counts from its
     * creation. An idle limit of zero or less is none.
     */
    public static boolean idle(Long lastUsedAt, Long createdAt, int idleSeconds, long now) {
        if (idleSeconds <= 0) {
            return false;
        }
        long since = lastUsedAt != null ? lastUsedAt : createdAt != null ? createdAt : now;
        return now - since > idleSeconds * 1000L;
    }

    /** Why a device's name cannot be one, or null. */
    public static String invalidName(String name) {
        if (name == null || name.isBlank()) {
            return "a device password needs the device's name";
        }
        if (name.length() > MAX_NAME) {
            return "the device's name is longer than " + MAX_NAME + " characters";
        }
        for (int i = 0; i < name.length(); i++) {
            if (Character.isISOControl(name.charAt(i))) {
                return "the device's name holds a control character";
            }
        }
        return null;
    }

    /** Why a list of ways cannot be a device password's, or null. */
    public static String invalidWays(List<String> ways) {
        if (ways == null || ways.isEmpty()) {
            return "a device password needs at least one way";
        }
        if (ways.size() > MAX_WAYS) {
            return "a device password names at most " + MAX_WAYS + " ways";
        }
        for (String w : ways) {
            if (w == null || !WAY.matcher(w).matches()) {
                return "not the name of a way: " + w;
            }
        }
        if (ways.stream().distinct().count() != ways.size()) {
            return "a way is named twice";
        }
        return null;
    }
}
