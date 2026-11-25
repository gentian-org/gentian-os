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

import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import java.util.List;
import org.keycloak.authentication.AuthenticationFlowContext;
import org.keycloak.authentication.AuthenticationFlowError;
import org.keycloak.authentication.authenticators.directgrant.AbstractDirectGrantAuthenticator;
import org.keycloak.common.util.Time;
import org.keycloak.events.Errors;
import org.keycloak.models.AuthenticationExecutionModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;
import org.keycloak.models.utils.KeycloakModelUtils;
import org.keycloak.provider.ProviderConfigProperty;

/**
 * The one step of the door's own sign-in flow: a username and a device
 * password, posted as a password grant by the door, and nothing else.
 *
 * <p>The operator binds this flow as the direct grant flow of the door's
 * device client alone. That client cannot check a person's password, and no
 * other client can check a device password: every other client's direct
 * grant runs Keycloak's own steps, which know the person's password only.
 *
 * <p>The ways the password may use are put on the session, from where a
 * user-session-note mapper puts them in the token ({@link #WAYS_NOTE}); the
 * door serves a path only if its way is among them.
 *
 * <p>A wrong password does not count towards Keycloak's lockout of the
 * person: the secret is not guessed, and counting would let anyone who knows
 * a username lock its holder out of the browser by presenting device
 * passwords. The door limits failed attempts itself.
 */
public class DevicePasswordAuthenticator extends AbstractDirectGrantAuthenticator {

    public static final String ID = "gentian-device-password";

    /** The session note that carries the ways, space-separated as scopes are. */
    public static final String WAYS_NOTE = "gentian.device_password.ways";

    /** The session note that names the password that was used. */
    public static final String ID_NOTE = "gentian.device_password.id";

    @Override
    public void authenticate(AuthenticationFlowContext context) {
        MultivaluedMap<String, String> form = context.getHttpRequest().getDecodedFormParameters();
        String username = form.getFirst("username");
        DevicePassword.Presented presented = DevicePassword.parse(form.getFirst("password"));
        RealmModel realm = context.getRealm();

        UserModel user = username == null || username.isBlank()
            ? null : KeycloakModelUtils.findUserByNameOrEmail(context.getSession(), realm, username.strip());
        if (user == null || presented == null) {
            refuse(context, Errors.INVALID_USER_CREDENTIALS);
            return;
        }
        if (!user.isEnabled()) {
            refuse(context, Errors.USER_DISABLED);
            return;
        }
        DevicePasswords.Held held = DevicePasswords.find(user, presented);
        if (held == null) {
            refuse(context, Errors.INVALID_USER_CREDENTIALS);
            return;
        }
        long now = Time.currentTimeMillis();
        if (DevicePassword.idle(held.data().lastUsedAt, held.model().getCreatedDate(),
            realm.getOfflineSessionIdleTimeout(), now)) {
            refuse(context, "device_password_idle");
            return;
        }

        DevicePasswords.markUsed(user, held, now);
        context.getAuthenticationSession().setUserSessionNote(WAYS_NOTE, String.join(" ", held.data().ways));
        context.getAuthenticationSession().setUserSessionNote(ID_NOTE, held.model().getId());
        context.getEvent().detail("device_password", held.model().getUserLabel());
        context.setUser(user);
        context.success(DevicePassword.TYPE);
    }

    /** One answer for every refusal: which part was wrong is nobody's to learn. */
    private void refuse(AuthenticationFlowContext context, String why) {
        context.getEvent().error(why);
        Response answer = errorResponse(Response.Status.UNAUTHORIZED.getStatusCode(),
            "invalid_grant", "Invalid user credentials");
        context.failure(AuthenticationFlowError.INVALID_CREDENTIALS, answer);
    }

    @Override
    public boolean requiresUser() {
        return false;
    }

    @Override
    public boolean configuredFor(KeycloakSession session, RealmModel realm, UserModel user) {
        return true;
    }

    @Override
    public void setRequiredActions(KeycloakSession session, RealmModel realm, UserModel user) {
    }

    @Override
    public boolean isUserSetupAllowed() {
        return false;
    }

    @Override
    public String getDisplayType() {
        return "Gentian device password";
    }

    @Override
    public String getReferenceCategory() {
        return DevicePassword.TYPE;
    }

    @Override
    public boolean isConfigurable() {
        return false;
    }

    @Override
    public AuthenticationExecutionModel.Requirement[] getRequirementChoices() {
        return new AuthenticationExecutionModel.Requirement[] {AuthenticationExecutionModel.Requirement.REQUIRED};
    }

    @Override
    public String getHelpText() {
        return "Checks a username and a device password posted as a password grant. Bind it to the door's device client only.";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return List.of();
    }

    @Override
    public String getId() {
        return ID;
    }
}
