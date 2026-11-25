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

import java.util.ArrayList;
import java.util.List;
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
 * Links a person who signs in to an app to the vouching entries the app's
 * own sign-in client names ({@value #LINK_TO_ATTRIBUTE}, comma-separated):
 * the app's own where it calls the tenant's home as the person, and the home
 * sync's where it keeps calendars or contacts the home sync keeps equal to
 * the home. The link is what the registrar's link route makes; here it is
 * made at sign-in, with nobody asked, because the person signing in to the
 * app is the consent.
 *
 * <p>An entry links only while it says so itself ({@value #LINK_CONFIG}),
 * which the platform writes while the app has a home to use. Without both
 * marks nothing happens.
 */
public class HomeLinkAtSignIn implements EventListenerProvider {

    private static final Logger LOG = Logger.getLogger(HomeLinkAtSignIn.class);

    /** The client attribute the platform writes the entries to link to into. */
    public static final String LINK_TO_ATTRIBUTE = "gentianos.io/link-to";

    /** The identity provider config entry that asks for links at sign-in. */
    public static final String LINK_CONFIG = "gentianos.io/link-at-sign-in";

    /** What VouchingAlias in the operator puts before a profile. */
    public static final String VOUCHING_PREFIX = "vouch-";

    private final KeycloakSession session;

    HomeLinkAtSignIn(KeycloakSession session) {
        this.session = session;
    }

    /**
     * The vouching entries a sign-in to a client with these attributes links
     * to: the attribute's entries that are vouching entries at all, without
     * repeats. Pure, so it is tested without a Keycloak.
     */
    static List<String> aliasesFor(Map<String, String> clientAttributes) {
        List<String> out = new ArrayList<>();
        if (clientAttributes == null) {
            return out;
        }
        String list = clientAttributes.get(LINK_TO_ATTRIBUTE);
        if (list == null) {
            return out;
        }
        for (String alias : list.split(",")) {
            String a = alias.trim();
            if (a.startsWith(VOUCHING_PREFIX) && a.length() > VOUCHING_PREFIX.length() && !out.contains(a)) {
                out.add(a);
            }
        }
        return out;
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
        List<String> aliases = client == null ? List.of() : aliasesFor(client.getAttributes());
        if (aliases.isEmpty()) {
            return;
        }
        UserModel user = session.users().getUserById(realm, event.getUserId());
        if (user == null || !user.isEnabled()) {
            return;
        }
        for (String alias : aliases) {
            IdentityProviderModel entry = session.identityProviders().getByAlias(alias);
            if (entry == null || !entry.isEnabled() || !linksAtSignIn(entry.getConfig())) {
                continue;
            }
            if (session.users().getFederatedIdentity(realm, user, alias) != null) {
                continue;
            }
            session.users().addFederatedIdentity(realm, user,
                new FederatedIdentityModel(alias, user.getId(), user.getUsername()));
            LOG.infof("linked user %s to %s at sign-in to %s in realm %s", user.getId(), alias, event.getClientId(), realm.getName());
        }
    }

    @Override
    public void onEvent(AdminEvent event, boolean includeRepresentation) {
    }

    @Override
    public void close() {
    }
}
