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

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Path;
import java.nio.charset.StandardCharsets;
import java.security.PrivateKey;
import java.security.Signature;
import java.time.Duration;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import io.gentianos.keycloak.events.DirectorEventListenerProviderFactory;
import org.jboss.logging.Logger;
import org.keycloak.Config;
import org.keycloak.util.JsonSerialization;

/**
 * Asks the platform which roles a person holds on an app and on the add-ons
 * activated in it. The question is signed as the listener's statements are:
 * {@code X-Gentian-Signature: keyid=<id>,t=<unix seconds>,sig=<base64>} over
 * {@code "<t>.<body>"}, Ed25519.
 *
 * <p>A sign-in waits on the answer, so it is asked for briefly and once:
 * whoever asked decides what an unanswered question means.
 */
final class RolesClient {

    private static final Duration TIMEOUT = Duration.ofSeconds(3);

    private final URI url;
    private final String keyId;
    private final PrivateKey key;
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2)).build();

    private static final Logger LOG = Logger.getLogger(RolesClient.class);

    /** Null while the hook is not configured: every token it is asked for then fails. */
    private static volatile RolesClient configured;

    /**
     * Reads where to ask and what to sign with from the configuration of the
     * {@code gentian-app-roles} mapper ({@code url}, {@code key-id},
     * {@code private-key-file}); the SAML mapper asks through the same
     * client. Unconfigured is said at start and is not fatal: a realm with
     * no client that carries a roles mapper never asks.
     */
    static void configure(Config.Scope config) {
        String url = config.get("url");
        String keyId = config.get("key-id");
        String keyFile = config.get("private-key-file");
        if (url == null || url.isBlank() || keyId == null || keyFile == null) {
            LOG.warn("gentian-app-roles is not configured (url, key-id, private-key-file): "
                + "a client that carries it signs nobody in");
            return;
        }
        try {
            PrivateKey key = DirectorEventListenerProviderFactory.readKey(Path.of(keyFile));
            configured = new RolesClient(url, keyId, key);
        } catch (Exception e) {
            throw new IllegalStateException("gentian-app-roles: cannot read " + keyFile, e);
        }
        LOG.infof("roles on apps are asked at %s, signed with key %s", url, keyId);
    }

    /** The configured client, or null. */
    static RolesClient configured() {
        return configured;
    }

    RolesClient(String url, String keyId, PrivateKey key) {
        this.url = URI.create(url);
        this.keyId = keyId;
        this.key = key;
    }

    /** What the platform answered: roles on apps, on a tenant, on the cluster. */
    @JsonIgnoreProperties(ignoreUnknown = true)
    static final class Answer {
        public List<AppRoles> apps;
        public List<String> tenant;
        public List<String> cluster;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    static final class AppRoles {
        public String app;
        public List<String> roles;
    }

    /** The question's body: which of tenant, app and cluster roles are asked for. */
    static String question(String tenant, String app, String clusterRealm, String person) throws IOException {
        Map<String, Object> body = new LinkedHashMap<>();
        if (tenant != null) {
            body.put("tenant", tenant);
        }
        if (app != null) {
            body.put("app", app);
        }
        if (clusterRealm != null) {
            body.put("cluster", true);
            body.put("realm", clusterRealm);
        }
        body.put("person", person);
        return JsonSerialization.writeValueAsString(body);
    }

    /** The question about one app. */
    static String question(String tenant, String app, String person) throws IOException {
        return question(tenant, app, null, person);
    }

    /** The whole answer; an answer that names no apps list is not one. */
    static Answer parseAnswer(String body) throws IOException {
        Answer answer = JsonSerialization.readValue(body, Answer.class);
        if (answer == null || answer.apps == null) {
            throw new IOException("the answer names no apps");
        }
        return answer;
    }

    /** The answer, as roles by profile name. */
    static Map<String, List<String>> parse(String body) throws IOException {
        Map<String, List<String>> out = new LinkedHashMap<>();
        for (AppRoles app : parseAnswer(body).apps) {
            if (app != null && app.app != null && app.roles != null) {
                out.put(app.app, app.roles);
            }
        }
        return out;
    }

    /** The roles held on an app and its add-ons, by profile name. */
    Map<String, List<String>> ask(String tenant, String app, String person) throws Exception {
        return parse(send(question(tenant, app, person)));
    }

    /** The cluster roles the account of a realm holds, by the model's names. */
    List<String> askCluster(String realm, String person) throws Exception {
        List<String> roles = parseAnswer(send(question(null, null, realm, person))).cluster;
        return roles == null ? List.of() : roles;
    }

    /** The roles a person holds on a tenant itself: admin, user. */
    List<String> askTenant(String tenant, String person) throws Exception {
        List<String> roles = parseAnswer(send(question(tenant, null, null, person))).tenant;
        return roles == null ? List.of() : roles;
    }

    /** Sends a question, signed; throws when it was not answered. */
    private String send(String payload) throws Exception {
        String timestamp = Long.toString(System.currentTimeMillis() / 1000);
        Signature signer = Signature.getInstance("Ed25519");
        signer.initSign(key);
        signer.update((timestamp + "." + payload).getBytes(StandardCharsets.UTF_8));
        String signature = Base64.getEncoder().encodeToString(signer.sign());

        HttpRequest request = HttpRequest.newBuilder(url)
            .timeout(TIMEOUT)
            .header("Content-Type", "application/json")
            .header("X-Gentian-Signature", "keyid=" + keyId + ",t=" + timestamp + ",sig=" + signature)
            .POST(HttpRequest.BodyPublishers.ofString(payload, StandardCharsets.UTF_8))
            .build();
        HttpResponse<String> response = http.send(request, HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8));
        if (response.statusCode() != 200) {
            throw new IOException("answered " + response.statusCode());
        }
        return response.body();
    }
}
