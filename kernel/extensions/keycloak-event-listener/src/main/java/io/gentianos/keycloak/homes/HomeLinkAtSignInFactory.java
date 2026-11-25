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

import org.keycloak.Config;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventListenerProviderFactory;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;

/**
 * The listener {@code gentian-home-links}. A realm lists it among its event
 * listeners for the people who sign in to an app that calls a home as the
 * person to be linked to that app's vouching entry.
 */
public class HomeLinkAtSignInFactory implements EventListenerProviderFactory {

    public static final String ID = "gentian-home-links";

    @Override
    public EventListenerProvider create(KeycloakSession session) {
        return new HomeLinkAtSignIn(session);
    }

    @Override
    public void init(Config.Scope config) {
    }

    @Override
    public void postInit(KeycloakSessionFactory factory) {
    }

    @Override
    public void close() {
    }

    @Override
    public String getId() {
        return ID;
    }
}
