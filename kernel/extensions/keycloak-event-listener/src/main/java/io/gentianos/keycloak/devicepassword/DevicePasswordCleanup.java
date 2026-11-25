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

import java.util.Map;
import org.jboss.logging.Logger;
import org.keycloak.events.Details;
import org.keycloak.events.Event;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.events.admin.OperationType;
import org.keycloak.events.admin.ResourceType;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;

/**
 * Removes a person's device passwords when what they were given under ends:
 * the person's password is changed or reset, by them or by an administrator,
 * or an administrator signs the person out of every session. It runs in the
 * transaction of the event, so the passwords are gone when it commits.
 *
 * <p>A person who is disabled or removed needs nothing here: the door's
 * sign-in step refuses a disabled person, and a removed one's credentials go
 * with them.
 */
public class DevicePasswordCleanup implements EventListenerProvider {

    private static final Logger LOG = Logger.getLogger(DevicePasswordCleanup.class);

    private final KeycloakSession session;

    DevicePasswordCleanup(KeycloakSession session) {
        this.session = session;
    }

    /** The person whose device passwords a user event ends, or null. */
    static String endedBy(EventType type, String userId, Map<String, String> details) {
        if (userId == null) {
            return null;
        }
        if (type == EventType.UPDATE_PASSWORD) {
            return userId;
        }
        if (type == EventType.UPDATE_CREDENTIAL && details != null
            && "password".equals(details.get(Details.CREDENTIAL_TYPE))) {
            return userId;
        }
        return null;
    }

    /** The person whose device passwords an admin event ends, or null. */
    static String endedBy(ResourceType resource, OperationType operation, String path) {
        if (resource != ResourceType.USER || operation != OperationType.ACTION || path == null) {
            return null;
        }
        // users/<id>/reset-password, users/<id>/logout
        String[] p = path.split("/");
        if (p.length == 3 && "users".equals(p[0]) && ("reset-password".equals(p[2]) || "logout".equals(p[2]))) {
            return p[1];
        }
        return null;
    }

    @Override
    public void onEvent(Event event) {
        clear(event.getRealmId(), endedBy(event.getType(), event.getUserId(), event.getDetails()));
    }

    @Override
    public void onEvent(AdminEvent event, boolean includeRepresentation) {
        clear(event.getRealmId(), endedBy(event.getResourceType(), event.getOperationType(), event.getResourcePath()));
    }

    private void clear(String realmId, String userId) {
        if (realmId == null || userId == null) {
            return;
        }
        RealmModel realm = session.realms().getRealm(realmId);
        UserModel user = realm == null ? null : session.users().getUserById(realm, userId);
        if (user == null) {
            return;
        }
        int n = DevicePasswords.removeAll(user);
        if (n > 0) {
            LOG.infof("removed %d device password(s) of user %s in realm %s", n, userId, realm.getName());
        }
    }

    @Override
    public void close() {
    }
}
