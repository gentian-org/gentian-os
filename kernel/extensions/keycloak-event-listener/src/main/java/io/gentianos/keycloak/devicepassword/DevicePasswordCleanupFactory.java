/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.devicepassword;

import org.keycloak.Config;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventListenerProviderFactory;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;

/**
 * The listener {@code gentian-device-passwords}. A realm lists it among its
 * event listeners, with admin events on, for its device passwords to end with
 * the person's password.
 */
public class DevicePasswordCleanupFactory implements EventListenerProviderFactory {

    public static final String ID = "gentian-device-passwords";

    @Override
    public EventListenerProvider create(KeycloakSession session) {
        return new DevicePasswordCleanup(session);
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
