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

import org.jboss.logging.Logger;
import org.keycloak.Config;
import org.keycloak.events.Event;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.events.log.JBossLoggingEventListenerProvider;
import org.keycloak.events.log.JBossLoggingEventListenerProviderFactory;
import org.keycloak.models.KeycloakSession;

/**
 * Keycloak's {@code jboss-logging} event listener, with a successful
 * {@code CLIENT_LOGIN} written at its own level.
 *
 * A {@code CLIENT_LOGIN} is a client taking a token for itself
 * (client credentials): the operator and provider-keycloak do it dozens of
 * times a minute, and at the success level every user event is logged at,
 * they are nearly all of Keycloak's output. Everything else is the built-in
 * listener unchanged: same logger ({@code org.keycloak.events}), same line,
 * same options. A failed client login keeps the error level.
 *
 * It takes the built-in's id, so every realm that lists {@code jboss-logging}
 * gets it with no change to the realm. Keycloak prefers a deployed factory
 * over its own for the same id; the higher order makes that explicit.
 *
 * Configuration (spi-events-listener-jboss-logging-*), beside the built-in's:
 *   client-login-level  level of a successful CLIENT_LOGIN (default debug)
 */
public class QuietClientLoginLoggingFactory extends JBossLoggingEventListenerProviderFactory {

    private static final Logger EVENTS = Logger.getLogger("org.keycloak.events");

    private Logger.Level clientLoginLevel;
    private Character quotes;
    private boolean sanitize;

    @Override
    public void init(Config.Scope config) {
        super.init(config);
        clientLoginLevel = Logger.Level.valueOf(config.get("client-login-level", "debug").toUpperCase());
        // The built-in keeps these private; read them the way it does, so a
        // client login is written in the same shape as every other line.
        sanitize = config.getBoolean("sanitize", true);
        String q = config.get("quotes", "\"");
        quotes = q.equals("none") ? null : q.length() == 1 ? q.charAt(0) : '"';
    }

    @Override
    public EventListenerProvider create(KeycloakSession session) {
        EventListenerProvider all = super.create(session);
        // Only successful client logins reach it, so its error level and
        // admin-event options are never used.
        EventListenerProvider clientLogins = new JBossLoggingEventListenerProvider(
            session, EVENTS, clientLoginLevel, clientLoginLevel, quotes, sanitize, false);
        return new EventListenerProvider() {
            @Override
            public void onEvent(Event event) {
                (isClientLogin(event) ? clientLogins : all).onEvent(event);
            }

            @Override
            public void onEvent(AdminEvent event, boolean includeRepresentation) {
                all.onEvent(event, includeRepresentation);
            }

            @Override
            public void close() {
                all.close();
                clientLogins.close();
            }
        };
    }

    static boolean isClientLogin(Event event) {
        return event.getType() == EventType.CLIENT_LOGIN && event.getError() == null;
    }

    @Override
    public int order() {
        return 1;
    }
}
