/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.binding;

import jakarta.ws.rs.core.MultivaluedMap;
import java.util.ArrayList;
import java.util.List;
import org.keycloak.http.HttpRequest;
import org.keycloak.models.AuthenticatedClientSessionModel;
import org.keycloak.models.ClientSessionContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.ProtocolMapperModel;
import org.keycloak.models.UserSessionModel;
import org.keycloak.protocol.oidc.mappers.AbstractOIDCProtocolMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAccessTokenMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAttributeMapperHelper;
import org.keycloak.protocol.oidc.mappers.OIDCIDTokenMapper;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.representations.IDToken;

/**
 * The protocol mapper {@code gentian-request-parameter-mapper}: puts a
 * client's bound request parameter ({@link RequestBinding}) in a claim.
 *
 * <p>Keycloak keeps a parameter it does not know from the authorization
 * request as a note of the client session ({@code client_request_param_<name>}).
 * That note is the session's, not the code's: a second sign-in of the same
 * person for the same client overwrites it. So the value is put in a token
 * only at the exchange of a code, and only when the value, as the session
 * now has it, computes the {@code redirect_uri} of that exchange -- which
 * Keycloak has by then held to the address the code was issued for. A code
 * issued for one value and exchanged after a sign-in for another gets no
 * claim; nor does a token from a refresh or any other grant, which has no
 * such address to compare with. Absent is the answer whenever anything is
 * missing, and a service that requires the claim then refuses the token.
 */
public final class RequestParameterMapper extends AbstractOIDCProtocolMapper
        implements OIDCAccessTokenMapper, OIDCIDTokenMapper {

    public static final String PROVIDER_ID = "gentian-request-parameter-mapper";

    /** Where Keycloak keeps an authorization request's own parameters (AuthorizationEndpoint). */
    static final String NOTE_PREFIX = "client_request_param_";

    private static final List<ProviderConfigProperty> CONFIG = new ArrayList<>();

    static {
        OIDCAttributeMapperHelper.addTokenClaimNameConfig(CONFIG);
        OIDCAttributeMapperHelper.addJsonTypeConfig(CONFIG);
        OIDCAttributeMapperHelper.addIncludeInTokensConfig(CONFIG, RequestParameterMapper.class);
    }

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public String getDisplayType() {
        return "Bound request parameter";
    }

    @Override
    public String getDisplayCategory() {
        return TOKEN_MAPPER_CATEGORY;
    }

    @Override
    public String getHelpText() {
        return "The client's bound request parameter, at the exchange of a code issued for the address computed from it.";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return CONFIG;
    }

    @Override
    protected void setClaim(IDToken token, ProtocolMapperModel mappingModel, UserSessionModel userSession,
                            KeycloakSession keycloakSession, ClientSessionContext clientSessionCtx) {
        if (clientSessionCtx == null) {
            return;
        }
        AuthenticatedClientSessionModel clientSession = clientSessionCtx.getClientSession();
        if (clientSession == null || clientSession.getClient() == null) {
            return;
        }
        RequestBinding binding = RequestBinding.of(clientSession.getClient().getAttributes());
        if (binding == null || !binding.isValid()) {
            return;
        }
        HttpRequest http = keycloakSession.getContext().getHttpRequest();
        MultivaluedMap<String, String> form = http == null ? null : http.getDecodedFormParameters();
        if (form == null) {
            return;
        }
        String value = claimValue(binding,
                clientSession.getNote(NOTE_PREFIX + binding.parameter()),
                RequestBinding.single(form.get("grant_type")),
                RequestBinding.single(form.get(RequestBoundRedirectExecutor.REDIRECT_URI)));
        if (value != null) {
            OIDCAttributeMapperHelper.mapClaim(token, mappingModel, value);
        }
    }

    /** The value to put in the token, or {@code null}. */
    static String claimValue(RequestBinding binding, String note, String grantType, String exchangeRedirect) {
        if (!"authorization_code".equals(grantType) || exchangeRedirect == null) {
            return null;
        }
        String computed = binding.redirectFor(note);
        return computed != null && computed.equals(exchangeRedirect) ? note : null;
    }
}
