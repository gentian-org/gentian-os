/*
 * Copyright 2026 Gentian Technologies.
 * Licensed under the Apache License, Version 2.0.
 */
package io.gentianos.keycloak.events;

import java.util.List;
import java.util.stream.Collectors;

import org.keycloak.events.Event;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.events.admin.OperationType;
import org.keycloak.events.admin.ResourceType;
import org.keycloak.models.GroupModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;

/**
 * Decides which Keycloak events can have changed a user's groups, and states
 * that user's groups when one happens.
 *
 * It always states the complete set, read from the model inside the
 * transaction that changed it, never "added X" or "removed Y". The receiver
 * compares it with what it holds, so an event delivered twice is harmless and
 * one that was lost is repaired by the next for that user — which, because a
 * login is such an event, is at the latest the next time they sign in.
 */
public class DirectorEventListenerProvider implements EventListenerProvider {

    private final KeycloakSession session;
    private final DirectorEventListenerProviderFactory factory;

    DirectorEventListenerProvider(KeycloakSession session, DirectorEventListenerProviderFactory factory) {
        this.session = session;
        this.factory = factory;
    }

    /**
     * User events. Groups change outside the admin API too — default groups at
     * registration, identity-provider and LDAP mappers at login — and none of
     * that produces an admin event.
     */
    @Override
    public void onEvent(Event event) {
        EventType type = event.getType();
        if (type != EventType.LOGIN && type != EventType.REGISTER
            && type != EventType.IDENTITY_PROVIDER_FIRST_LOGIN) {
            return;
        }
        RealmModel realm = session.realms().getRealm(event.getRealmId());
        if (realm == null || event.getUserId() == null) {
            return;
        }
        state(realm, session.users().getUserById(realm, event.getUserId()));
    }

    @Override
    public void onEvent(AdminEvent event, boolean includeRepresentation) {
        RealmModel realm = session.realms().getRealm(event.getRealmId());
        if (realm == null || event.getResourcePath() == null) {
            return;
        }
        String[] path = event.getResourcePath().split("/");
        ResourceType resource = event.getResourceType();
        OperationType operation = event.getOperationType();

        if (resource == ResourceType.GROUP_MEMBERSHIP && path.length >= 2 && "users".equals(path[0])) {
            // users/<id>/groups/<group id>, on both join and leave
            state(realm, session.users().getUserById(realm, path[1]));
        } else if (resource == ResourceType.USER && operation == OperationType.CREATE
            && path.length >= 2 && "users".equals(path[0])) {
            // a user can be created with groups already in the representation
            state(realm, session.users().getUserById(realm, path[1]));
        } else if (resource == ResourceType.GROUP && operation == OperationType.UPDATE
            && path.length >= 2 && "groups".equals(path[0])) {
            // A rename changes the name every member holds. Each member's new
            // complete set drops the old name as a side effect.
            GroupModel group = session.groups().getGroupById(realm, path[1]);
            if (group != null && group.getParentId() == null) {
                session.users().getGroupMembersStream(realm, group).forEach(member -> state(realm, member));
            }
        }
        // Deletions of groups and users are handled from the model's removal
        // events, in the factory: here their content is already gone.
    }

    private void state(RealmModel realm, UserModel user) {
        if (user == null) {
            return;
        }
        // Top-level groups only. The platform's roles are top-level groups by
        // name; a subgroup anyone may have named the same is not one of them.
        List<String> groups = user.getGroupsStream()
            .filter(group -> group.getParentId() == null)
            .map(GroupModel::getName)
            .sorted()
            .collect(Collectors.toList());
        factory.afterCommit(session, Payload.userMemberships(realm.getName(), user.getId(), groups));
    }

    @Override
    public void close() {
    }
}
