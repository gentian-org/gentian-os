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
import org.keycloak.models.FederatedIdentityModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;

/**
 * Tells the platform that a person came into a realm through an identity
 * provider: the cluster's administrators entering a tenant, people of one
 * tenant signing in to another. What such a person holds in the tenant is
 * the rights store's, and the platform gives it when they arrive; the realm
 * only says that they did, through which provider, and who they are there:
 * the id of their account at the provider, as the realm's link to it holds
 * it. For a tenant's provider that is the person's id in the other tenant's
 * realm, which is how the platform asks what they hold there.
 *
 * <p>A person who makes an account on the realm's registration page is told
 * too, so that the platform gives it what the tenant names for sign-ups. An
 * account made for a person who came in through a provider is not such an
 * account, and is not told as one.
 *
 * <p>A person who is removed is told too, from the model's own removal event
 * (the factory), so that what they held goes with them.
 */
public class DirectorEventListenerProvider implements EventListenerProvider {

    /** The event detail that names the identity provider a sign-in came through. */
    static final String PROVIDER_DETAIL = "identity_provider";
    /** The event detail that says how an account was registered, and the value for one a provider brought. */
    static final String REGISTER_METHOD_DETAIL = "register_method";
    static final String BROKER_REGISTRATION = "broker";

    private final KeycloakSession session;
    private final DirectorEventListenerProviderFactory factory;

    DirectorEventListenerProvider(KeycloakSession session, DirectorEventListenerProviderFactory factory) {
        this.session = session;
        this.factory = factory;
    }

    @Override
    public void onEvent(Event event) {
        EventType type = event.getType();
        if (type == EventType.REGISTER) {
            onRegister(event);
            return;
        }
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
        factory.afterCommit(session,
            Payload.userEntered(realm.getName(), event.getUserId(), provider, providerUser(realm, event.getUserId(), provider)));
    }

    /** A person made an account themself: one that came through no provider. */
    private void onRegister(Event event) {
        if (!madeThemself(event.getDetails()) || event.getUserId() == null) {
            return;
        }
        RealmModel realm = session.realms().getRealm(event.getRealmId());
        if (realm == null) {
            return;
        }
        factory.afterCommit(session, Payload.userRegistered(realm.getName(), event.getUserId()));
    }

    /**
     * The person's account id at the provider, from the realm's link to it, or
     * null when the realm holds no such link: the platform then gives nothing
     * that depends on who they are there.
     */
    private String providerUser(RealmModel realm, String userId, String provider) {
        UserModel user = session.users().getUserById(realm, userId);
        if (user == null) {
            return null;
        }
        FederatedIdentityModel link = session.users().getFederatedIdentity(realm, user, provider);
        if (link == null || link.getUserId() == null || link.getUserId().isBlank()) {
            return null;
        }
        return link.getUserId();
    }

    /** The identity provider a sign-in came through, or null for one that came through none. Pure. */
    static String providerOf(Map<String, String> details) {
        if (details == null) {
            return null;
        }
        String provider = details.get(PROVIDER_DETAIL);
        return provider == null || provider.isBlank() ? null : provider;
    }

    /**
     * Whether a registration is of an account the person made on the
     * registration page, and not one made for a person who came in through
     * an identity provider (which Keycloak reports as a registration too). Pure.
     */
    static boolean madeThemself(Map<String, String> details) {
        if (providerOf(details) != null) {
            return false;
        }
        return details == null || !BROKER_REGISTRATION.equals(details.get(REGISTER_METHOD_DETAIL));
    }

    @Override
    public void onEvent(AdminEvent event, boolean includeRepresentation) {
    }

    @Override
    public void close() {
    }
}
