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

import jakarta.ws.rs.Consumes;
import jakarta.ws.rs.POST;
import jakarta.ws.rs.Path;
import jakarta.ws.rs.PathParam;
import jakarta.ws.rs.Produces;
import jakarta.ws.rs.core.MediaType;
import jakarta.ws.rs.core.Response;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;
import org.keycloak.services.managers.AppAuthManager;
import org.keycloak.services.managers.AuthenticationManager.AuthResult;
import org.keycloak.services.resource.RealmResourceProvider;
import org.keycloak.services.resources.admin.AdminAuth;
import org.keycloak.services.resources.admin.fgap.AdminPermissionEvaluator;
import org.keycloak.services.resources.admin.fgap.AdminPermissions;

/**
 * {@code POST /realms/<realm>/gentian-device-passwords/users/<id>}: makes a
 * device password for a person and returns it, once.
 *
 * <p>The caller is the platform's registrar, with its credential for this
 * realm, after it established that the person asked for the password
 * themselves and signed in moments before. Keycloak's admin permissions
 * decide here as on any admin endpoint: managing the user is required. The
 * admin API lists and removes device passwords as it does every credential.
 */
public class DevicePasswordResource implements RealmResourceProvider {

    private final KeycloakSession session;

    public DevicePasswordResource(KeycloakSession session) {
        this.session = session;
    }

    @Override
    public Object getResource() {
        return this;
    }

    @Override
    public void close() {
    }

    /** Body of a request for a device password. */
    public static class CreateRequest {
        public String name;
        public List<String> ways;
    }

    @POST
    @Path("users/{id}")
    @Consumes(MediaType.APPLICATION_JSON)
    @Produces(MediaType.APPLICATION_JSON)
    public Response create(@PathParam("id") String id, CreateRequest body) {
        RealmModel realm = session.getContext().getRealm();
        AdminPermissionEvaluator permissions = authorize(realm);
        if (permissions == null) {
            return Response.status(Response.Status.UNAUTHORIZED).build();
        }
        UserModel user = session.users().getUserById(realm, id);
        if (user == null) {
            return Response.status(Response.Status.NOT_FOUND).build();
        }
        permissions.users().requireManage(user);
        if (!user.isEnabled()) {
            return error("the person is disabled");
        }
        CreateRequest req = body == null ? new CreateRequest() : body;
        String why = DevicePassword.invalidName(req.name);
        if (why == null) {
            why = DevicePassword.invalidWays(req.ways);
        }
        if (why != null) {
            return error(why);
        }
        DevicePasswords.Created created;
        try {
            created = DevicePasswords.create(user, req.name, req.ways);
        } catch (IllegalArgumentException e) {
            return error(e.getMessage());
        }
        Map<String, Object> out = new LinkedHashMap<>();
        out.put("id", created.id());
        out.put("name", created.name());
        out.put("ways", created.ways());
        out.put("username", user.getUsername());
        out.put("password", created.password());
        out.put("createdAt", created.createdAt());
        return Response.status(Response.Status.CREATED).entity(out).build();
    }

    /** A bearer token of this realm, evaluated as the admin API evaluates one. */
    private AdminPermissionEvaluator authorize(RealmModel realm) {
        AuthResult auth = new AppAuthManager.BearerTokenAuthenticator(session).setRealm(realm).authenticate();
        if (auth == null) {
            return null;
        }
        ClientModel client = realm.getClientByClientId(auth.token().getIssuedFor());
        return AdminPermissions.evaluator(session, realm, new AdminAuth(realm, auth.token(), auth.user(), client));
    }

    private static Response error(String message) {
        return Response.status(Response.Status.BAD_REQUEST).entity(Map.of("error", message)).build();
    }
}
