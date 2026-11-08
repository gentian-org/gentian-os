/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.binding;

/**
 * A value to match that stops the match when it has been read too often.
 *
 * <p>Java's regular expressions backtrack, and a pattern of nested
 * repetitions can take exponential time on a value made for it. The pattern
 * is a profile's and the value is whoever sends the request's, so a match
 * reads its characters through this, and one that reads more than
 * {@link #BUDGET} of them is abandoned -- and counts as no match.
 */
final class BoundedChars implements CharSequence {

    /** Character reads one match may make: generous for any sane pattern on 512 characters. */
    static final int BUDGET = 200_000;

    static final class TooMuchWork extends RuntimeException {
        private static final long serialVersionUID = 1L;

        TooMuchWork() {
            super("the pattern took too long on this value", null, false, false);
        }
    }

    private final String value;
    private final int[] reads;

    BoundedChars(String value) {
        this(value, new int[1]);
    }

    private BoundedChars(String value, int[] reads) {
        this.value = value;
        this.reads = reads;
    }

    @Override
    public char charAt(int index) {
        if (++reads[0] > BUDGET) {
            throw new TooMuchWork();
        }
        return value.charAt(index);
    }

    @Override
    public int length() {
        return value.length();
    }

    @Override
    public CharSequence subSequence(int start, int end) {
        return new BoundedChars(value.substring(start, end), reads);
    }

    @Override
    public String toString() {
        return value;
    }
}
