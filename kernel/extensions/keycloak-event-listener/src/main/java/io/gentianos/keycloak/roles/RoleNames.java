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

import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.TreeSet;

/**
 * What a person's roles are called inside an app. Pure, so it is tested
 * without a Keycloak.
 *
 * <p>{@code inApp} is the platform's: for the app and each add-on activated
 * in it, by profile name, what each role is called inside the app.
 * {@code held} is the rights store's answer: the roles the person holds, by
 * the same profile names.
 */
final class RoleNames {

    private RoleNames() {
    }

    /** The in-app names of the roles held: sorted, each once. */
    static List<String> held(Map<String, Map<String, List<String>>> inApp, Map<String, List<String>> held) {
        TreeSet<String> out = new TreeSet<>();
        if (inApp == null || held == null) {
            return new ArrayList<>(out);
        }
        for (Map.Entry<String, List<String>> app : held.entrySet()) {
            Map<String, List<String>> names = inApp.get(app.getKey());
            if (names == null || app.getValue() == null) {
                continue;
            }
            for (String role : app.getValue()) {
                List<String> called = names.get(role);
                if (called != null) {
                    out.addAll(called);
                }
            }
        }
        return new ArrayList<>(out);
    }

    /**
     * The roles held in the platform's own names, for an app whose profile
     * gives them none inside the app: the app's roles as they are (user,
     * admin, a declared one), an add-on's behind its profile name
     * ({@code odoo-sales-ce/admin}). Sorted, each once.
     */
    static List<String> platform(String app, Map<String, List<String>> held) {
        TreeSet<String> out = new TreeSet<>();
        if (held != null) {
            for (Map.Entry<String, List<String>> entry : held.entrySet()) {
                if (entry.getValue() == null) {
                    continue;
                }
                for (String role : entry.getValue()) {
                    out.add(entry.getKey().equals(app) ? role : entry.getKey() + "/" + role);
                }
            }
        }
        return new ArrayList<>(out);
    }

    /** Every in-app name the platform decides: sorted, each once. */
    static List<String> mapped(Map<String, Map<String, List<String>>> inApp) {
        TreeSet<String> out = new TreeSet<>();
        if (inApp != null) {
            for (Map<String, List<String>> names : inApp.values()) {
                if (names == null) {
                    continue;
                }
                for (List<String> called : names.values()) {
                    if (called != null) {
                        out.addAll(called);
                    }
                }
            }
        }
        return new ArrayList<>(out);
    }

    /**
     * Whether a token that cannot be given the store's answer may carry the
     * roles of the one before. Only the exchange of a code is a sign-in; a
     * renewal, or a read of the person's details with a token already
     * issued, keeps people at work.
     */
    static boolean mayKeepPrevious(String grantType) {
        return !"authorization_code".equals(grantType);
    }
}
