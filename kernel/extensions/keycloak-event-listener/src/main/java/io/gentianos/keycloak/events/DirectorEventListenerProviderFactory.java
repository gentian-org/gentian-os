/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.events;

import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyFactory;
import java.security.PrivateKey;
import java.security.spec.PKCS8EncodedKeySpec;
import java.util.Base64;

import org.jboss.logging.Logger;
import org.keycloak.Config;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventListenerProviderFactory;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.models.UserModel;

/**
 * Tells the platform what only the realm sees happen to a person: that they
 * came in through an identity provider, and that they were removed. Who holds
 * what is the rights store's; these two statements are what it cannot learn
 * any other way. Each is signed, to the one receiver this listener is
 * configured with. It holds a signing key and nothing else -- no token, no
 * credential the platform would accept for any other call.
 *
 * Configuration (spi-events-listener-gentian-director-*):
 *   director-url      where events are posted
 *   key-id            the id the director knows the public key under
 *   private-key-file  PKCS#8 PEM Ed25519 private key
 */
public class DirectorEventListenerProviderFactory implements EventListenerProviderFactory {

    public static final String ID = "gentian-director";
    private static final Logger LOG = Logger.getLogger(DirectorEventListenerProviderFactory.class);

    private Sender sender;

    @Override
    public String getId() {
        return ID;
    }

    @Override
    public void init(Config.Scope config) {
        String url = config.get("director-url");
        String keyId = config.get("key-id");
        String keyFile = config.get("private-key-file");
        if (url == null || keyId == null || keyFile == null) {
            // Enabled in a realm but not configured: say so at start, where it
            // is seen, instead of dropping every event where it is not.
            throw new IllegalStateException(
                "gentian-director event listener needs director-url, key-id and private-key-file");
        }
        try {
            sender = new Sender(url, keyId, readKey(Path.of(keyFile)));
        } catch (Exception e) {
            throw new IllegalStateException("gentian-director event listener: cannot read " + keyFile, e);
        }
        LOG.infof("events go to %s, signed with key %s", url, keyId);
    }

    public static PrivateKey readKey(Path file) throws Exception {
        String pem = Files.readString(file)
            .replace("-----BEGIN PRIVATE KEY-----", "")
            .replace("-----END PRIVATE KEY-----", "")
            .replaceAll("\\s", "");
        return KeyFactory.getInstance("Ed25519")
            .generatePrivate(new PKCS8EncodedKeySpec(Base64.getDecoder().decode(pem)));
    }

    /**
     * A removal is told from the model's own removal event, which fires
     * while the account can still be read.
     */
    @Override
    public void postInit(KeycloakSessionFactory factory) {
        factory.register(event -> {
            if (event instanceof UserModel.UserRemovedEvent) {
                UserModel.UserRemovedEvent removed = (UserModel.UserRemovedEvent) event;
                afterCommit(removed.getKeycloakSession(),
                    Payload.userDeleted(removed.getRealm().getName(), removed.getUser().getId()));
            }
        });
    }

    /** Nothing is said about a change until the transaction that made it has committed. */
    void afterCommit(KeycloakSession session, String payload) {
        session.getTransactionManager().enlistAfterCompletion(new AfterCommit(() -> sender.enqueue(payload)));
    }

    @Override
    public EventListenerProvider create(KeycloakSession session) {
        return new DirectorEventListenerProvider(session, this);
    }

    @Override
    public void close() {
        if (sender != null) {
            sender.close();
        }
    }
}
