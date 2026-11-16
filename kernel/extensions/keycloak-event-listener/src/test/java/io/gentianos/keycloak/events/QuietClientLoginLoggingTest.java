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

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.Test;
import org.keycloak.events.Event;
import org.keycloak.events.EventType;
import org.keycloak.events.log.JBossLoggingEventListenerProviderFactory;

/** Only a successful client login is quieter; it replaces the built-in listener by id. */
class QuietClientLoginLoggingTest {

    private static Event event(EventType type, String error) {
        Event e = new Event();
        e.setType(type);
        e.setError(error);
        return e;
    }

    @Test
    void aSuccessfulClientLoginIsTheOnlyEventLoggedApart() {
        assertTrue(QuietClientLoginLoggingFactory.isClientLogin(event(EventType.CLIENT_LOGIN, null)));
        assertFalse(QuietClientLoginLoggingFactory.isClientLogin(event(EventType.CLIENT_LOGIN_ERROR, "invalid_client_credentials")));
        assertFalse(QuietClientLoginLoggingFactory.isClientLogin(event(EventType.CLIENT_LOGIN, "invalid_client")));
        assertFalse(QuietClientLoginLoggingFactory.isClientLogin(event(EventType.LOGIN, null)));
        assertFalse(QuietClientLoginLoggingFactory.isClientLogin(event(EventType.REFRESH_TOKEN, null)));
    }

    @Test
    void itTakesTheBuiltInListenersPlace() {
        QuietClientLoginLoggingFactory f = new QuietClientLoginLoggingFactory();
        assertEquals(JBossLoggingEventListenerProviderFactory.ID, f.getId());
        assertTrue(f.order() > new JBossLoggingEventListenerProviderFactory().order());
    }
}
