/*
 * Copyright 2026 Gentian Technologies.
 * Licensed under the Apache License, Version 2.0.
 */
package io.gentianos.keycloak.events;

import java.util.List;
import java.util.UUID;

/** The three statements the director understands, as JSON. */
final class Payload {

    private Payload() {
    }

    static String userMemberships(String realm, String user, List<String> groups) {
        StringBuilder json = head("user.memberships", realm).append(",\"user\":").append(quote(user)).append(",\"groups\":[");
        for (int i = 0; i < groups.size(); i++) {
            json.append(i == 0 ? "" : ",").append(quote(groups.get(i)));
        }
        return json.append("]}").toString();
    }

    static String userDeleted(String realm, String user) {
        return head("user.deleted", realm).append(",\"user\":").append(quote(user)).append('}').toString();
    }

    static String groupDeleted(String realm, String group) {
        return head("group.deleted", realm).append(",\"group\":").append(quote(group)).append('}').toString();
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
