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
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import org.jboss.logging.Logger;
import org.keycloak.dom.saml.v2.assertion.AttributeStatementType;
import org.keycloak.dom.saml.v2.assertion.AttributeType;
import org.keycloak.models.AuthenticatedClientSessionModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.ProtocolMapperModel;
import org.keycloak.models.UserSessionModel;
import org.keycloak.protocol.saml.mappers.AbstractSAMLProtocolMapper;
import org.keycloak.protocol.saml.mappers.SAMLAttributeStatementMapper;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.saml.common.constants.JBossSAMLURIConstants;
import org.keycloak.util.JsonSerialization;

/**
 * The protocol mapper {@code gentian-saml-app-roles}: what
 * {@link AppRolesMapper} writes into a token, written into a SAML answer as
 * one attribute, for an app the platform signs people in to through its
 * sidecar. The attribute lists the in-app names of the roles the person
 * holds; a person who holds none gets no attribute.
 *
 * <p>A SAML answer is always a sign-in: when the rights store cannot be
 * asked, it fails.
 */
public final class SamlAppRolesMapper extends AbstractSAMLProtocolMapper implements SAMLAttributeStatementMapper {

    public static final String PROVIDER_ID = "gentian-saml-app-roles";

    static final String ATTRIBUTE_NAME = "attribute.name";

    private static final Logger LOG = Logger.getLogger(SamlAppRolesMapper.class);
    private static final List<ProviderConfigProperty> CONFIG = new ArrayList<>();
    private static final TypeReference<Map<String, Map<String, List<String>>>> IN_APP = new TypeReference<>() { };

    static {
        CONFIG.add(new ProviderConfigProperty(ATTRIBUTE_NAME, "Attribute", "The attribute the roles are listed in.",
                ProviderConfigProperty.STRING_TYPE, "Role"));
        CONFIG.add(new ProviderConfigProperty(AppRolesMapper.TENANT, "Tenant", "The tenant the app is installed in.",
                ProviderConfigProperty.STRING_TYPE, null));
        CONFIG.add(new ProviderConfigProperty(AppRolesMapper.APP, "App", "The app's profile name.",
                ProviderConfigProperty.STRING_TYPE, null));
        CONFIG.add(new ProviderConfigProperty(AppRolesMapper.ROLES, "Roles inside the app",
                "JSON: by profile name and role, what the role is called inside the app.",
                ProviderConfigProperty.STRING_TYPE, null));
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
        return "Role Mapper";
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
    public void transformAttributeStatement(AttributeStatementType attributeStatement, ProtocolMapperModel mappingModel,
                                            KeycloakSession session, UserSessionModel userSession,
                                            AuthenticatedClientSessionModel clientSession) {
        Map<String, String> config = mappingModel.getConfig();
        String attribute = config.getOrDefault(ATTRIBUTE_NAME, "Role");
        String tenant = config.get(AppRolesMapper.TENANT);
        String app = config.get(AppRolesMapper.APP);
        RolesClient client = RolesClient.configured();
        if (tenant == null || app == null || userSession == null || userSession.getUser() == null || client == null) {
            throw new IllegalStateException("the roles hook of this client is incomplete");
        }
        List<String> names;
        try {
            Map<String, Map<String, List<String>>> inApp =
                JsonSerialization.readValue(config.getOrDefault(AppRolesMapper.ROLES, "{}"), IN_APP);
            names = RoleNames.held(inApp, client.ask(tenant, app, userSession.getUser().getId()));
        } catch (InterruptedException stop) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("the rights store cannot be asked");
        } catch (Exception e) {
            LOG.warnf("roles of a person on %s/%s not answered: %s", tenant, app, e.getMessage());
            throw new IllegalStateException("the rights store cannot be asked");
        }
        if (names.isEmpty()) {
            return;
        }
        AttributeType roles = new AttributeType(attribute);
        roles.setNameFormat(JBossSAMLURIConstants.ATTRIBUTE_FORMAT_BASIC.get());
        for (String name : names) {
            roles.addAttributeValue(name);
        }
        attributeStatement.addAttribute(new AttributeStatementType.ASTChoiceType(roles));
    }
}
