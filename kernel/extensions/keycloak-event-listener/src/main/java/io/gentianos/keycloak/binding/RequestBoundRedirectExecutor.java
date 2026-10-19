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

import java.util.List;
import java.util.Map;
import org.keycloak.OAuthErrorException;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.protocol.oidc.endpoints.request.AuthorizationEndpointRequest;
import org.keycloak.representations.idm.ClientPolicyExecutorConfigurationRepresentation;
import org.keycloak.services.clientpolicy.ClientPolicyContext;
import org.keycloak.services.clientpolicy.ClientPolicyException;
import org.keycloak.services.clientpolicy.context.AuthorizationRequestContext;
import org.keycloak.services.clientpolicy.context.PreAuthorizationRequestContext;
import org.keycloak.services.clientpolicy.executor.ClientPolicyExecutorProvider;

/**
 * The client-policy executor {@code gentian-request-bound-redirect}: a code
 * for a client that declares a {@link RequestBinding} goes only to the
 * address computed from the request's parameter.
 *
 * <p>Such a client is registered with {@code https://*}, so Keycloak's own
 * redirect check admits any https address for it, and this narrows that to
 * one. It refuses ({@code invalid_request}) unless the request carries the
 * parameter once, the value is one the binding allows, and
 * {@code redirect_uri} -- given once -- is, character for character, the
 * address computed from it.
 *
 * <p>Twice. Before Keycloak has parsed the request, from its parameters as
 * sent: a refusal there is an error page, where a refusal after the redirect
 * check would send the browser, with the error, to the address that was
 * just refused. A request object or a pushed request is refused there
 * outright, since what it carries is not what that check read. And again
 * once the request is parsed, against what Keycloak itself took from it and
 * the address its redirect check settled on, so that a request that passed
 * the first check and reads differently to Keycloak is still refused.
 */
public final class RequestBoundRedirectExecutor
        implements ClientPolicyExecutorProvider<ClientPolicyExecutorConfigurationRepresentation> {

    static final String REDIRECT_URI = "redirect_uri";

    private final KeycloakSession session;

    RequestBoundRedirectExecutor(KeycloakSession session) {
        this.session = session;
    }

    @Override
    public String getProviderId() {
        return RequestBoundRedirectExecutorFactory.PROVIDER_ID;
    }

    @Override
    public void executeOnEvent(ClientPolicyContext context) throws ClientPolicyException {
        switch (context.getEvent()) {
            case PRE_AUTHORIZATION_REQUEST -> {
                PreAuthorizationRequestContext pre = (PreAuthorizationRequestContext) context;
                RealmModel realm = session.getContext().getRealm();
                ClientModel client = realm == null ? null : realm.getClientByClientId(pre.getClientId());
                if (client == null) {
                    return; // Keycloak refuses an unknown client itself, next.
                }
                refuseUnless(sentRefusal(RequestBinding.of(client.getAttributes()), pre.getRequestParameters()));
            }
            case AUTHORIZATION_REQUEST -> {
                AuthorizationRequestContext auth = (AuthorizationRequestContext) context;
                AuthorizationEndpointRequest request = auth.getAuthorizationEndpointRequest();
                refuseUnless(parsedRefusal(
                        RequestBinding.of(auth.getClient().getAttributes()),
                        request.getAdditionalReqParams(),
                        request.getRedirectUri(),
                        auth.getRedirectUri()));
            }
            default -> {
            }
        }
    }

    private static void refuseUnless(String refusal) throws ClientPolicyException {
        if (refusal != null) {
            throw new ClientPolicyException(OAuthErrorException.INVALID_REQUEST, refusal);
        }
    }

    /**
     * Why a request as sent is refused, or {@code null}: the first check.
     * {@code binding} null is a client that declares none.
     */
    static String sentRefusal(RequestBinding binding, Map<String, List<String>> params) {
        if (binding == null) {
            return null;
        }
        if (!binding.isValid()) {
            return "the client's request parameter cannot be read";
        }
        if (params.containsKey("request") || params.containsKey("request_uri")) {
            return "this client does not take a request object or a pushed request";
        }
        String computed = binding.redirectFor(RequestBinding.single(params.get(binding.parameter())));
        if (computed == null) {
            return "the request carries no valid " + binding.parameter();
        }
        if (!computed.equals(RequestBinding.single(params.get(REDIRECT_URI)))) {
            return "redirect_uri is not the address for this " + binding.parameter();
        }
        return null;
    }

    /** Why a parsed request is refused, or {@code null}: the second check. */
    static String parsedRefusal(RequestBinding binding, Map<String, String> additional, String redirectParam, String verifiedRedirect) {
        if (binding == null) {
            return null;
        }
        if (!binding.isValid()) {
            return "the client's request parameter cannot be read";
        }
        String computed = binding.redirectFor(additional == null ? null : additional.get(binding.parameter()));
        if (computed == null) {
            return "the request carries no valid " + binding.parameter();
        }
        if (!computed.equals(redirectParam) || !computed.equals(verifiedRedirect)) {
            return "redirect_uri is not the address for this " + binding.parameter();
        }
        return null;
    }
}
