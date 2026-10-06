/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.activation;

import jakarta.ws.rs.Consumes;
import jakarta.ws.rs.POST;
import jakarta.ws.rs.Path;
import jakarta.ws.rs.PathParam;
import jakarta.ws.rs.Produces;
import jakarta.ws.rs.core.MediaType;
import jakarta.ws.rs.core.Response;
import jakarta.ws.rs.core.UriBuilder;
import java.util.List;
import java.util.Map;
import java.util.Set;
import org.keycloak.authentication.actiontoken.execactions.ExecuteActionsActionToken;
import org.keycloak.common.util.Time;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;
import org.keycloak.protocol.oidc.utils.RedirectUtils;
import org.keycloak.services.managers.AppAuthManager;
import org.keycloak.services.managers.AuthenticationManager.AuthResult;
import org.keycloak.services.resource.RealmResourceProvider;
import org.keycloak.services.resources.LoginActionsService;
import org.keycloak.services.resources.admin.AdminAuth;
import org.keycloak.services.resources.admin.permissions.AdminPermissionEvaluator;
import org.keycloak.services.resources.admin.permissions.AdminPermissions;

/**
 * Returns the link Keycloak would otherwise only mail.
 *
 * <p>An administrator who sets their own password through a single-use,
 * expiring link is the practice the platform already follows for every member
 * it invites. Keycloak's admin API can only send that link; it cannot hand it
 * back. Where nobody can be mailed -- an administrator created without a
 * recovery address, or a cluster whose mail is not configured yet -- the link
 * has to be shown to whoever created the account, once. This endpoint does
 * that and nothing more: the same action token {@code execute-actions-email}
 * builds, with the same lifespan, the same redirect checks, and the same
 * permission ({@code manage-users} on the user, as the admin API decides it).
 *
 * <p>The caller's token may come from the realm itself (the director's
 * per-realm credential) or from master (the installer, before any other
 * credential exists). Either way the admin API's own evaluator decides.
 */
public class ActivationLinkResource implements RealmResourceProvider {

    /** Only the actions an account needs to become usable. */
    private static final Set<String> ALLOWED_ACTIONS =
            Set.of("UPDATE_PASSWORD", "CONFIGURE_TOTP", "VERIFY_EMAIL", "webauthn-register");

    private final KeycloakSession session;

    public ActivationLinkResource(KeycloakSession session) {
        this.session = session;
    }

    @Override
    public Object getResource() {
        return this;
    }

    @Override
    public void close() {
    }

    /** Body of a link request; every field optional. */
    public static class LinkRequest {
        public List<String> actions;
        public Integer lifespan;
        public String clientId;
        public String redirectUri;
    }

    @POST
    @Path("users/{id}/link")
    @Consumes(MediaType.APPLICATION_JSON)
    @Produces(MediaType.APPLICATION_JSON)
    public Response link(@PathParam("id") String id, LinkRequest body) {
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
            return error(Response.Status.BAD_REQUEST, "the user is disabled");
        }

        LinkRequest req = body == null ? new LinkRequest() : body;
        List<String> actions = req.actions == null || req.actions.isEmpty()
                ? List.of("UPDATE_PASSWORD") : req.actions;
        for (String a : actions) {
            if (!ALLOWED_ACTIONS.contains(a)) {
                return error(Response.Status.BAD_REQUEST, "not an activation action: " + a);
            }
        }

        // The lifespan an admin-issued action token gets anyway, unless the
        // caller asks for a shorter one. Never a longer one: a link shown on a
        // screen is at least as exposed as one in a mailbox.
        int realmLifespan = realm.getActionTokenGeneratedByAdminLifespan();
        int lifespan = req.lifespan == null || req.lifespan <= 0
                ? realmLifespan : Math.min(req.lifespan, realmLifespan);

        String clientId = req.clientId;
        String redirectUri = req.redirectUri;
        if (clientId != null) {
            ClientModel client = realm.getClientByClientId(clientId);
            if (client == null || !client.isEnabled()) {
                return error(Response.Status.BAD_REQUEST, "no such client: " + clientId);
            }
            if (redirectUri != null) {
                String verified = RedirectUtils.verifyRedirectUri(session, redirectUri, client);
                if (verified == null) {
                    return error(Response.Status.BAD_REQUEST, "redirect_uri is not one the client accepts");
                }
                redirectUri = verified;
            }
        } else {
            redirectUri = null;
        }

        int expiration = Time.currentTime() + lifespan;
        ExecuteActionsActionToken token = new ExecuteActionsActionToken(
                user.getId(), user.getEmail(), expiration, actions, redirectUri, clientId);
        UriBuilder builder = LoginActionsService.actionTokenProcessor(session.getContext().getUri());
        builder.queryParam("key", token.serialize(session, realm, session.getContext().getUri()));
        String link = builder.build(realm.getName()).toString();

        return Response.ok(Map.of("link", link, "expiresAt", expiration, "actions", actions)).build();
    }

    /**
     * The admin API's own authentication: a bearer token from this realm or
     * from master, evaluated by the same permissions an admin endpoint uses.
     */
    private AdminPermissionEvaluator authorize(RealmModel target) {
        AuthResult auth = new AppAuthManager.BearerTokenAuthenticator(session).setRealm(target).authenticate();
        RealmModel tokenRealm = target;
        if (auth == null) {
            RealmModel master = session.realms().getRealmByName("master");
            if (master == null) {
                return null;
            }
            // The signature key is looked up in the realm of the session
            // CONTEXT, not the one handed to the authenticator: this
            // endpoint lives under /realms/<target>, so without the switch a
            // master token was checked against master's issuer and the
            // target's keys, failed, and every installer call answered 401.
            // Keycloak's own AdminRoot switches the same way.
            session.getContext().setRealm(master);
            try {
                auth = new AppAuthManager.BearerTokenAuthenticator(session).setRealm(master).authenticate();
            } finally {
                session.getContext().setRealm(target);
            }
            tokenRealm = master;
        }
        if (auth == null) {
            return null;
        }
        ClientModel client = tokenRealm.getClientByClientId(auth.getToken().getIssuedFor());
        AdminAuth admin = new AdminAuth(tokenRealm, auth.getToken(), auth.getUser(), client);
        return AdminPermissions.evaluator(session, target, admin);
    }

    private static Response error(Response.Status status, String message) {
        return Response.status(status).entity(Map.of("error", message)).build();
    }
}
