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
import org.keycloak.Config;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.services.clientpolicy.executor.ClientPolicyExecutorProvider;
import org.keycloak.services.clientpolicy.executor.ClientPolicyExecutorProviderFactory;

/**
 * Registers {@link RequestBoundRedirectExecutor}. It takes no configuration:
 * what it holds a client to is the client's own attributes
 * ({@link RequestBinding}), so one policy of the realm serves every client.
 */
public final class RequestBoundRedirectExecutorFactory implements ClientPolicyExecutorProviderFactory {

    public static final String PROVIDER_ID = "gentian-request-bound-redirect";

    @Override
    public ClientPolicyExecutorProvider<?> create(KeycloakSession session) {
        return new RequestBoundRedirectExecutor(session);
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
        return PROVIDER_ID;
    }

    @Override
    public String getHelpText() {
        return "A client whose attributes bind a request parameter: its code goes only to the redirect address computed from that parameter.";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return List.of();
    }
}
