/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.captcha;

import jakarta.ws.rs.core.MultivaluedMap;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import org.keycloak.Config;
import org.keycloak.authentication.FormAction;
import org.keycloak.authentication.FormActionFactory;
import org.keycloak.authentication.FormContext;
import org.keycloak.authentication.ValidationContext;
import org.keycloak.events.Errors;
import org.keycloak.forms.login.LoginFormsProvider;
import org.keycloak.models.AuthenticationExecutionModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;
import org.keycloak.models.utils.FormMessage;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.sessions.AuthenticationSessionModel;

/**
 * The registration form's proof-of-work check, {@code gentian-pow-captcha}.
 *
 * <p>In place of Keycloak's reCAPTCHA, which sends every visitor to Google.
 * This one asks the browser for a second of arithmetic instead
 * ({@link PowChallenge}) and asks nobody else anything: a sovereignty
 * platform's sign-up page loads nothing from a third party.
 *
 * <p>The challenge is made when the page is built and kept on the
 * authentication session, which is the one place the browser's later answer
 * can be matched with it: the page carries the salt, the browser finds the
 * nonce, the form brings it back, and the session says what the salt was.
 * Solved once, the challenge is gone from the session, so an answer is good
 * for the registration it was made for and no other.
 */
public class PowCaptchaFormActionFactory implements FormAction, FormActionFactory {

    public static final String ID = "gentian-pow-captcha";

    /** The form field the browser's answer arrives in. */
    public static final String FIELD = "gentian_pow";

    /** What the template reads: {@code gentianPow.salt} and {@code gentianPow.bits}. */
    public static final String ATTRIBUTE = "gentianPow";

    /** The message key the theme answers a failed check with. */
    public static final String FAILED = "gentianPowFailed";

    private static final String NOTE_SALT = "gentian.pow.salt";
    private static final String NOTE_BITS = "gentian.pow.bits";
    private static final String NOTE_EXPIRES = "gentian.pow.expires";

    private static final AuthenticationExecutionModel.Requirement[] REQUIREMENT_CHOICES = {
        AuthenticationExecutionModel.Requirement.REQUIRED,
        AuthenticationExecutionModel.Requirement.DISABLED
    };

    @Override
    public void buildPage(FormContext context, LoginFormsProvider form) {
        AuthenticationSessionModel session = context.getAuthenticationSession();
        String salt = PowChallenge.newSalt();
        int bits = PowChallenge.DEFAULT_BITS;
        session.setAuthNote(NOTE_SALT, salt);
        session.setAuthNote(NOTE_BITS, Integer.toString(bits));
        session.setAuthNote(NOTE_EXPIRES, Long.toString(now() + PowChallenge.TTL_SECONDS));
        form.setAttribute(ATTRIBUTE, Map.of("salt", salt, "bits", Integer.toString(bits)));
    }

    @Override
    public void validate(ValidationContext context) {
        MultivaluedMap<String, String> formData = context.getHttpRequest().getDecodedFormParameters();
        AuthenticationSessionModel session = context.getAuthenticationSession();
        String salt = session.getAuthNote(NOTE_SALT);
        String bits = session.getAuthNote(NOTE_BITS);
        String expires = session.getAuthNote(NOTE_EXPIRES);
        // Whatever the answer, the challenge is spent: a second try gets a
        // new page and a new challenge, never another go at this one.
        session.removeAuthNote(NOTE_SALT);
        session.removeAuthNote(NOTE_BITS);
        session.removeAuthNote(NOTE_EXPIRES);

        if (salt != null && bits != null && expires != null
                && now() <= parseLong(expires)
                && PowChallenge.solves(salt, formData.getFirst(FIELD), (int) parseLong(bits))) {
            context.success();
            return;
        }
        List<FormMessage> errors = new ArrayList<>();
        errors.add(new FormMessage(null, FAILED));
        context.error(Errors.INVALID_REGISTRATION);
        context.validationError(formData, errors);
        context.excludeOtherErrors();
    }

    private static long parseLong(String s) {
        try {
            return Long.parseLong(s);
        } catch (NumberFormatException e) {
            return -1;
        }
    }

    private static long now() {
        return System.currentTimeMillis() / 1000;
    }

    @Override
    public void success(FormContext context) {
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
    public String getDisplayType() {
        return "Gentian proof-of-work check";
    }

    @Override
    public String getReferenceCategory() {
        return "captcha";
    }

    @Override
    public boolean isConfigurable() {
        return false;
    }

    @Override
    public AuthenticationExecutionModel.Requirement[] getRequirementChoices() {
        return REQUIREMENT_CHOICES;
    }

    @Override
    public boolean isUserSetupAllowed() {
        return false;
    }

    @Override
    public String getHelpText() {
        return "Asks the browser for a second of arithmetic before the account is made, "
            + "so a scripted sign-up costs its author more than it costs the realm. "
            + "Asks no third party anything.";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return List.of();
    }

    @Override
    public FormAction create(KeycloakSession session) {
        return this;
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
