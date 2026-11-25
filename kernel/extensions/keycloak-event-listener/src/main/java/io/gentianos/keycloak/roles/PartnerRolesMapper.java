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

import java.util.ArrayList;
import java.util.List;
import org.jboss.logging.Logger;
import org.keycloak.models.ClientSessionContext;
import org.keycloak.models.FederatedIdentityModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.ProtocolMapperModel;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;
import org.keycloak.models.UserSessionModel;
import org.keycloak.protocol.oidc.mappers.AbstractOIDCProtocolMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAccessTokenMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAttributeMapperHelper;
import org.keycloak.protocol.oidc.mappers.TokenIntrospectionTokenMapper;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.representations.IDToken;

/**
 * The protocol mapper {@code gentian-partner-roles}: what a partner assertion
 * says of the person's role, under the claim named. {@value #TENANT_ADMIN}
 * for a person who administers the tenant; {@value #CLUSTER_ADMIN} for a
 * person who administers the cluster -- an account of the kernel realm, or
 * one that came into the tenant's realm from it and is linked to the kernel
 * account that does. A person who is neither has an empty list.
 *
 * <p>Both are the rights store's to say and are asked for every token. When
 * the store cannot be asked the token is not written: an assertion without
 * the role would read as a person who holds none.
 */
public final class PartnerRolesMapper extends AbstractOIDCProtocolMapper
        implements OIDCAccessTokenMapper, TokenIntrospectionTokenMapper {

    public static final String PROVIDER_ID = "gentian-partner-roles";

    static final String TENANT_ADMIN = "tenant-admin";
    static final String CLUSTER_ADMIN = "cluster-admin";

    /** The realm the cluster's administrators are accounts of. */
    static final String KERNEL_REALM = "gentian.kernel-realm";
    /** The identity provider a tenant's realm lets them in through. */
    static final String KERNEL_PROVIDER = "kernel";

    private static final Logger LOG = Logger.getLogger(PartnerRolesMapper.class);
    private static final List<ProviderConfigProperty> CONFIG = new ArrayList<>();

    static {
        OIDCAttributeMapperHelper.addTokenClaimNameConfig(CONFIG);
        OIDCAttributeMapperHelper.addIncludeInTokensConfig(CONFIG, PartnerRolesMapper.class);
        CONFIG.add(new ProviderConfigProperty(AppRolesMapper.TENANT, "Tenant", "The tenant the partner serves.",
                ProviderConfigProperty.STRING_TYPE, null));
        CONFIG.add(new ProviderConfigProperty(KERNEL_REALM, "Kernel realm",
                "The realm the cluster's administrators are accounts of.", ProviderConfigProperty.STRING_TYPE, null));
    }

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public String getDisplayType() {
        return "Partner roles";
    }

    @Override
    public String getDisplayCategory() {
        return TOKEN_MAPPER_CATEGORY;
    }

    @Override
    public String getHelpText() {
        return "Whether the person administers the tenant or the cluster, as the platform's rights store answers.";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return CONFIG;
    }

    @Override
    protected void setClaim(IDToken token, ProtocolMapperModel mappingModel, UserSessionModel userSession,
                            KeycloakSession keycloakSession, ClientSessionContext clientSessionCtx) {
        String claim = mappingModel.getConfig().get(OIDCAttributeMapperHelper.TOKEN_CLAIM_NAME);
        String tenant = mappingModel.getConfig().get(AppRolesMapper.TENANT);
        String kernelRealm = mappingModel.getConfig().get(KERNEL_REALM);
        RolesClient client = RolesClient.configured();
        if (claim == null || claim.isBlank() || tenant == null || userSession == null
                || userSession.getUser() == null || userSession.getRealm() == null || client == null) {
            throw ClusterRolesMapper.unavailable("the roles hook of this client is incomplete");
        }
        RealmModel realm = userSession.getRealm();
        UserModel user = userSession.getUser();
        try {
            List<String> tenantRoles = client.askTenant(tenant, user.getId());
            List<String> clusterRoles = List.of();
            String kernelPerson = kernelAccount(keycloakSession, realm, user, kernelRealm);
            if (kernelPerson != null) {
                clusterRoles = client.askCluster(kernelRealm, kernelPerson);
            }
            token.getOtherClaims().put(claim, roles(tenantRoles, clusterRoles));
        } catch (InterruptedException stop) {
            Thread.currentThread().interrupt();
            throw ClusterRolesMapper.unavailable("the rights store cannot be asked; try again in a moment");
        } catch (Exception e) {
            LOG.warnf("partner roles of a person of %s not answered: %s", tenant, e.getMessage());
            throw ClusterRolesMapper.unavailable("the rights store cannot be asked; try again in a moment");
        }
    }

    /**
     * The id of the kernel realm's account this person is: their own in the
     * kernel realm, the linked one where they came in through the kernel's
     * identity provider, null for anybody else.
     */
    private static String kernelAccount(KeycloakSession session, RealmModel realm, UserModel user, String kernelRealm) {
        if (kernelRealm == null || kernelRealm.isBlank()) {
            return null;
        }
        if (kernelRealm.equals(realm.getName())) {
            return user.getId();
        }
        FederatedIdentityModel link = session.users().getFederatedIdentity(realm, user, KERNEL_PROVIDER);
        return link == null ? null : link.getUserId();
    }

    /** The assertion's roles from the store's answers. Pure. */
    static List<String> roles(List<String> tenantRoles, List<String> clusterRoles) {
        List<String> out = new ArrayList<>();
        if (clusterRoles != null && clusterRoles.contains("admin")) {
            out.add(CLUSTER_ADMIN);
        }
        if (tenantRoles != null && tenantRoles.contains("admin")) {
            out.add(TENANT_ADMIN);
        }
        return out;
    }
}
