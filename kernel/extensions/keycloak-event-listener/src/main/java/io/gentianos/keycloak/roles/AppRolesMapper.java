/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.roles;

import com.fasterxml.jackson.core.type.TypeReference;
import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import org.jboss.logging.Logger;
import org.keycloak.Config;
import org.keycloak.http.HttpRequest;
import org.keycloak.models.ClientSessionContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.ProtocolMapperModel;
import org.keycloak.models.UserSessionModel;
import org.keycloak.protocol.oidc.mappers.AbstractOIDCProtocolMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAccessTokenMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAttributeMapperHelper;
import org.keycloak.protocol.oidc.mappers.OIDCIDTokenMapper;
import org.keycloak.protocol.oidc.mappers.UserInfoTokenMapper;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.representations.IDToken;
import org.keycloak.services.ErrorResponseException;
import org.keycloak.util.JsonSerialization;

/**
 * The protocol mapper {@code gentian-app-roles}: writes a person's roles on
 * the app they sign in to into the app's token, in the app's own names.
 *
 * <p>Who holds what is the rights store's to say, so the store is asked for
 * every token ({@link RolesClient}); nothing of it is kept in the realm. The
 * platform writes on the mapper which app it is ({@value #TENANT},
 * {@value #APP}) and what each role is called inside it ({@value #ROLES}).
 * Two claims are written: the claim named, with the in-app names of the
 * roles held, and that name followed by {@value #MAPPED_SUFFIX}, with every
 * in-app name the platform decides -- so that the app takes away what is no
 * longer held and leaves alone what was never the platform's.
 *
 * <p>When the store cannot be asked, a sign-in fails: a token without roles
 * would read as a person who holds none. A token that is renewed, or a read
 * of the person's details with a token already issued, carries the roles of
 * the one before, kept on the person's session: people at work stay at work.
 */
public final class AppRolesMapper extends AbstractOIDCProtocolMapper
        implements OIDCAccessTokenMapper, OIDCIDTokenMapper, UserInfoTokenMapper {

    public static final String PROVIDER_ID = "gentian-app-roles";

    static final String TENANT = "gentian.tenant";
    static final String APP = "gentian.app";
    static final String ROLES = "gentian.roles";
    static final String MAPPED_SUFFIX = "_mapped";

    /** Where the last answer is kept on the person's session, by client. */
    static final String NOTE_PREFIX = "gentian.app-roles.";

    private static final Logger LOG = Logger.getLogger(AppRolesMapper.class);
    private static final List<ProviderConfigProperty> CONFIG = new ArrayList<>();
    private static final TypeReference<Map<String, Map<String, List<String>>>> IN_APP = new TypeReference<>() { };
    private static final TypeReference<Map<String, List<String>>> HELD = new TypeReference<>() { };

    static {
        OIDCAttributeMapperHelper.addTokenClaimNameConfig(CONFIG);
        OIDCAttributeMapperHelper.addIncludeInTokensConfig(CONFIG, AppRolesMapper.class);
        CONFIG.add(new ProviderConfigProperty(TENANT, "Tenant", "The tenant the app is installed in.",
                ProviderConfigProperty.STRING_TYPE, null));
        CONFIG.add(new ProviderConfigProperty(APP, "App", "The app's profile name.",
                ProviderConfigProperty.STRING_TYPE, null));
        CONFIG.add(new ProviderConfigProperty(ROLES, "Roles inside the app",
                "JSON: by profile name and role, what the role is called inside the app.",
                ProviderConfigProperty.STRING_TYPE, null));
    }

    @Override
    public void init(Config.Scope config) {
        RolesClient.configure(config);
    }

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public String getDisplayType() {
        return "Roles on the app";
    }

    @Override
    public String getDisplayCategory() {
        return TOKEN_MAPPER_CATEGORY;
    }

    @Override
    public String getHelpText() {
        return "The roles the person holds on this app, as the platform's rights store answers, in the app's own names.";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return CONFIG;
    }

    @Override
    protected void setClaim(IDToken token, ProtocolMapperModel mappingModel, UserSessionModel userSession,
                            KeycloakSession keycloakSession, ClientSessionContext clientSessionCtx) {
        Map<String, String> config = mappingModel.getConfig();
        String claim = config.get(OIDCAttributeMapperHelper.TOKEN_CLAIM_NAME);
        String tenant = config.get(TENANT);
        String app = config.get(APP);
        if (claim == null || claim.isBlank() || tenant == null || app == null || userSession == null
                || userSession.getUser() == null) {
            throw unavailable("the roles hook of this client is incomplete");
        }
        Map<String, Map<String, List<String>>> inApp;
        try {
            inApp = JsonSerialization.readValue(config.getOrDefault(ROLES, "{}"), IN_APP);
        } catch (Exception e) {
            throw unavailable("the roles hook of this client cannot be read");
        }
        Map<String, List<String>> held = held(keycloakSession, userSession, tenant, app);
        token.getOtherClaims().put(claim, RoleNames.held(inApp, held));
        token.getOtherClaims().put(claim + MAPPED_SUFFIX, RoleNames.mapped(inApp));
    }

    /**
     * The store's answer for this person and app: asked once per request,
     * however many tokens the request writes, and kept on the session for a
     * renewal that cannot ask.
     */
    @SuppressWarnings("unchecked")
    private Map<String, List<String>> held(KeycloakSession session, UserSessionModel userSession, String tenant, String app) {
        String person = userSession.getUser().getId();
        String key = NOTE_PREFIX + tenant + "/" + app;
        Object asked = session.getAttribute(key + "/" + person);
        if (asked instanceof Map) {
            return (Map<String, List<String>>) asked;
        }
        Map<String, List<String>> held = null;
        RolesClient c = RolesClient.configured();
        if (c != null) {
            try {
                held = c.ask(tenant, app, person);
                userSession.setNote(key, JsonSerialization.writeValueAsString(held));
            } catch (InterruptedException stop) {
                Thread.currentThread().interrupt();
            } catch (Exception e) {
                LOG.warnf("roles of a person on %s/%s not answered: %s", tenant, app, e.getMessage());
            }
        }
        if (held == null) {
            String previous = userSession.getNote(key);
            if (previous == null || !RoleNames.mayKeepPrevious(grantType(session))) {
                throw unavailable("the rights store cannot be asked; try again in a moment");
            }
            try {
                held = JsonSerialization.readValue(previous, HELD);
            } catch (Exception e) {
                throw unavailable("the rights store cannot be asked; try again in a moment");
            }
        }
        session.setAttribute(key + "/" + person, held);
        return held;
    }

    private static String grantType(KeycloakSession session) {
        HttpRequest http = session.getContext().getHttpRequest();
        if (http == null || !"POST".equalsIgnoreCase(http.getHttpMethod())) {
            return null;
        }
        MultivaluedMap<String, String> form = http.getDecodedFormParameters();
        return form == null ? null : form.getFirst("grant_type");
    }

    private static ErrorResponseException unavailable(String description) {
        return new ErrorResponseException("temporarily_unavailable", description, Response.Status.SERVICE_UNAVAILABLE);
    }
}
