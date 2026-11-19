/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */

package io.gentianos.keycloak.homes;

import java.util.Map;

import org.jboss.logging.Logger;
import org.keycloak.events.Event;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.models.ClientModel;
import org.keycloak.models.FederatedIdentityModel;
import org.keycloak.models.IdentityProviderModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;

/**
 * Links a person who signs in to an app that calls the tenant's home as the
 * person (integrations[].asPerson) to that app's vouching entry, so that the
 * app may later ask for the person's token at the home. The link is what the
 * registrar's link route makes; here it is made at sign-in, with nobody asked,
 * because the person signing in to the app is the consent.
 *
 * <p>Two marks decide it, both written by the platform: the app's own sign-in
 * client names its profile ({@value #PROFILE_ATTRIBUTE}), and the vouching
 * entry vouch-&lt;profile&gt; says it links at sign-in ({@value #LINK_CONFIG})
 * while the tenant has granted the app a home. Without either nothing happens.
 */
public class HomeLinkAtSignIn implements EventListenerProvider {

    private static final Logger LOG = Logger.getLogger(HomeLinkAtSignIn.class);

    /** The client attribute the platform writes the app's profile into. */
    public static final String PROFILE_ATTRIBUTE = "gentianos.io/profile";

    /** The identity provider config entry that asks for links at sign-in. */
    public static final String LINK_CONFIG = "gentianos.io/link-at-sign-in";

    /** What VouchingAlias in the operator puts before a profile. */
    public static final String VOUCHING_PREFIX = "vouch-";

    private final KeycloakSession session;

    HomeLinkAtSignIn(KeycloakSession session) {
        this.session = session;
    }

    /**
     * The vouching entry a sign-in to a client with these attributes links to,
     * or null. Pure, so it is tested without a Keycloak.
     */
    static String aliasFor(Map<String, String> clientAttributes) {
        if (clientAttributes == null) {
            return null;
        }
        String profile = clientAttributes.get(PROFILE_ATTRIBUTE);
        if (profile == null || profile.isBlank()) {
            return null;
        }
        return VOUCHING_PREFIX + profile.trim();
    }

    /** Whether a vouching entry with this config links at sign-in. */
    static boolean linksAtSignIn(Map<String, String> config) {
        return config != null && "true".equals(config.get(LINK_CONFIG));
    }

    @Override
    public void onEvent(Event event) {
        if (event.getType() != EventType.LOGIN || event.getUserId() == null || event.getClientId() == null) {
            return;
        }
        RealmModel realm = session.realms().getRealm(event.getRealmId());
        if (realm == null) {
            return;
        }
        ClientModel client = realm.getClientByClientId(event.getClientId());
        String alias = client == null ? null : aliasFor(client.getAttributes());
        if (alias == null) {
            return;
        }
        IdentityProviderModel entry = session.identityProviders().getByAlias(alias);
        if (entry == null || !entry.isEnabled() || !linksAtSignIn(entry.getConfig())) {
            return;
        }
        UserModel user = session.users().getUserById(realm, event.getUserId());
        if (user == null || !user.isEnabled()) {
            return;
        }
        if (session.users().getFederatedIdentity(realm, user, alias) != null) {
            return;
        }
        session.users().addFederatedIdentity(realm, user,
            new FederatedIdentityModel(alias, user.getId(), user.getUsername()));
        LOG.infof("linked user %s to %s at sign-in to %s in realm %s", user.getId(), alias, event.getClientId(), realm.getName());
    }

    @Override
    public void onEvent(AdminEvent event, boolean includeRepresentation) {
    }

    @Override
    public void close() {
    }
}
