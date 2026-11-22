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
import org.jboss.logging.Logger;
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
 * The protocol mapper {@code gentian-cluster-roles}: writes the cluster roles
 * a person holds into a token of the kernel realm, as a list under the claim
 * named, by the names the platform's rights model gives them ({@code admin},
 * {@code auditor}, ...). It is what the cluster's own tools read where they
 * would read a groups claim: the vault, the Kubernetes API's proxy, Argo CD,
 * Grafana, and a tenant's realm that lets the cluster's administrators in.
 *
 * <p>The roles are the rights store's to say and are asked for every token,
 * as {@link AppRolesMapper} asks for an app's: a sign-in fails when the store
 * cannot be asked, and a renewed token carries the roles of the one before.
 */
public final class ClusterRolesMapper extends AbstractOIDCProtocolMapper
        implements OIDCAccessTokenMapper, OIDCIDTokenMapper, UserInfoTokenMapper {

    public static final String PROVIDER_ID = "gentian-cluster-roles";

    static final String NOTE = "gentian.cluster-roles";

    private static final Logger LOG = Logger.getLogger(ClusterRolesMapper.class);
    private static final List<ProviderConfigProperty> CONFIG = new ArrayList<>();
    private static final TypeReference<List<String>> ROLES = new TypeReference<>() { };

    static {
        OIDCAttributeMapperHelper.addTokenClaimNameConfig(CONFIG);
        OIDCAttributeMapperHelper.addIncludeInTokensConfig(CONFIG, ClusterRolesMapper.class);
    }

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public String getDisplayType() {
        return "Roles on the cluster";
    }

    @Override
    public String getDisplayCategory() {
        return TOKEN_MAPPER_CATEGORY;
    }

    @Override
    public String getHelpText() {
        return "The cluster roles the person holds, as the platform's rights store answers.";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return CONFIG;
    }

    @Override
    protected void setClaim(IDToken token, ProtocolMapperModel mappingModel, UserSessionModel userSession,
                            KeycloakSession keycloakSession, ClientSessionContext clientSessionCtx) {
        String claim = mappingModel.getConfig().get(OIDCAttributeMapperHelper.TOKEN_CLAIM_NAME);
        if (claim == null || claim.isBlank() || userSession == null || userSession.getUser() == null
                || userSession.getRealm() == null) {
            throw unavailable("the roles hook of this client is incomplete");
        }
        token.getOtherClaims().put(claim, held(keycloakSession, userSession));
    }

    /** Asked once per request, and kept on the session for a renewal that cannot ask. */
    @SuppressWarnings("unchecked")
    static List<String> held(KeycloakSession session, UserSessionModel userSession) {
        String realm = userSession.getRealm().getName();
        String person = userSession.getUser().getId();
        Object asked = session.getAttribute(NOTE + "/" + person);
        if (asked instanceof List) {
            return (List<String>) asked;
        }
        List<String> held = null;
        RolesClient client = RolesClient.configured();
        if (client != null) {
            try {
                held = client.askCluster(realm, person);
                userSession.setNote(NOTE, JsonSerialization.writeValueAsString(held));
            } catch (InterruptedException stop) {
                Thread.currentThread().interrupt();
            } catch (Exception e) {
                LOG.warnf("cluster roles of a person of realm %s not answered: %s", realm, e.getMessage());
            }
        }
        if (held == null) {
            String previous = userSession.getNote(NOTE);
            if (previous == null || !RoleNames.mayKeepPrevious(grantType(session))) {
                throw unavailable("the rights store cannot be asked; try again in a moment");
            }
            try {
                held = JsonSerialization.readValue(previous, ROLES);
            } catch (Exception e) {
                throw unavailable("the rights store cannot be asked; try again in a moment");
            }
        }
        session.setAttribute(NOTE + "/" + person, held);
        return held;
    }

    static String grantType(KeycloakSession session) {
        HttpRequest http = session.getContext().getHttpRequest();
        if (http == null || !"POST".equalsIgnoreCase(http.getHttpMethod())) {
            return null;
        }
        MultivaluedMap<String, String> form = http.getDecodedFormParameters();
        return form == null ? null : form.getFirst("grant_type");
    }

    static ErrorResponseException unavailable(String description) {
        return new ErrorResponseException("temporarily_unavailable", description, Response.Status.SERVICE_UNAVAILABLE);
    }
}
