/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.events;

import java.util.UUID;

/** The statements the platform understands, as JSON. */
final class Payload {

    private Payload() {
    }

    /** providerUser is who the person is at the provider, or null when the realm does not say. */
    static String userEntered(String realm, String user, String provider, String providerUser) {
        StringBuilder out = head("user.entered", realm).append(",\"user\":").append(quote(user))
            .append(",\"provider\":").append(quote(provider));
        if (providerUser != null) {
            out.append(",\"providerUser\":").append(quote(providerUser));
        }
        return out.append('}').toString();
    }

    static String userRegistered(String realm, String user) {
        return head("user.registered", realm).append(",\"user\":").append(quote(user)).append('}').toString();
    }

    static String userDeleted(String realm, String user) {
        return head("user.deleted", realm).append(",\"user\":").append(quote(user)).append('}').toString();
    }

    private static StringBuilder head(String type, String realm) {
        return new StringBuilder(256)
            .append("{\"id\":").append(quote(UUID.randomUUID().toString()))
            .append(",\"time\":").append(System.currentTimeMillis())
            .append(",\"realm\":").append(quote(realm))
            .append(",\"type\":").append(quote(type));
    }

    /** JSON string quoting. Group names are whatever an administrator typed. */
    static String quote(String s) {
        StringBuilder out = new StringBuilder(s.length() + 2).append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"': out.append("\\\""); break;
                case '\\': out.append("\\\\"); break;
                case '\n': out.append("\\n"); break;
                case '\r': out.append("\\r"); break;
                case '\t': out.append("\\t"); break;
                default:
                    if (c < 0x20 || c == 0x2028 || c == 0x2029) {
                        out.append(String.format("\\u%04x", (int) c));
                    } else {
                        out.append(c);
                    }
            }
        }
        return out.append('"').toString();
    }
}
