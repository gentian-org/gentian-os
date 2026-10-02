package io.gentianos.keycloak.activation;

import org.keycloak.Config;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.services.resource.RealmResourceProvider;
import org.keycloak.services.resource.RealmResourceProviderFactory;

/**
 * Registers {@code /realms/<realm>/gentian-activation}: the activation link an
 * administrator is handed when nobody can be mailed one.
 */
public class ActivationLinkResourceProviderFactory implements RealmResourceProviderFactory {

    public static final String ID = "gentian-activation";

    @Override
    public RealmResourceProvider create(KeycloakSession session) {
        return new ActivationLinkResource(session);
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
