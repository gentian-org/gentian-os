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

import java.util.Map;

import org.keycloak.events.Event;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;

/**
 * Tells the platform that a person came into a realm through an identity
 * provider: the cluster's administrators entering a tenant, people of one
 * tenant signing in to another. What such a person holds in the tenant is
 * the rights store's, and the platform gives it when they arrive; the realm
 * only says that they did, and through which provider.
 *
 * <p>A person who is removed is told too, from the model's own removal event
 * (the factory), so that what they held goes with them.
 */
public class DirectorEventListenerProvider implements EventListenerProvider {

    /** The event detail that names the identity provider a sign-in came through. */
    static final String PROVIDER_DETAIL = "identity_provider";

    private final KeycloakSession session;
    private final DirectorEventListenerProviderFactory factory;

    DirectorEventListenerProvider(KeycloakSession session, DirectorEventListenerProviderFactory factory) {
        this.session = session;
        this.factory = factory;
    }

    @Override
    public void onEvent(Event event) {
        EventType type = event.getType();
        if (type != EventType.LOGIN && type != EventType.IDENTITY_PROVIDER_FIRST_LOGIN) {
            return;
        }
        String provider = providerOf(event.getDetails());
        if (provider == null || event.getUserId() == null) {
            return;
        }
        RealmModel realm = session.realms().getRealm(event.getRealmId());
        if (realm == null) {
            return;
        }
        factory.afterCommit(session, Payload.userEntered(realm.getName(), event.getUserId(), provider));
    }

    /** The identity provider a sign-in came through, or null for one that came through none. Pure. */
    static String providerOf(Map<String, String> details) {
        if (details == null) {
            return null;
        }
        String provider = details.get(PROVIDER_DETAIL);
        return provider == null || provider.isBlank() ? null : provider;
    }

    @Override
    public void onEvent(AdminEvent event, boolean includeRepresentation) {
    }

    @Override
    public void close() {
    }
}
