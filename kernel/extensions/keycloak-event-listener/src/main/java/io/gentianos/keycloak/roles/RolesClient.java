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
import java.nio.charset.StandardCharsets;
import java.security.PrivateKey;
import java.security.Signature;
import java.time.Duration;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
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

    RolesClient(String url, String keyId, PrivateKey key) {
        this.url = URI.create(url);
        this.keyId = keyId;
        this.key = key;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    static final class Answer {
        public List<AppRoles> apps;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    static final class AppRoles {
        public String app;
        public List<String> roles;
    }

    /** The question's body. */
    static String question(String tenant, String app, String person) throws IOException {
        Map<String, String> body = new LinkedHashMap<>();
        body.put("tenant", tenant);
        body.put("app", app);
        body.put("person", person);
        return JsonSerialization.writeValueAsString(body);
    }

    /** The answer, as roles by profile name. */
    static Map<String, List<String>> parse(String body) throws IOException {
        Answer answer = JsonSerialization.readValue(body, Answer.class);
        Map<String, List<String>> out = new LinkedHashMap<>();
        if (answer == null || answer.apps == null) {
            throw new IOException("the answer names no apps");
        }
        for (AppRoles app : answer.apps) {
            if (app != null && app.app != null && app.roles != null) {
                out.put(app.app, app.roles);
            }
        }
        return out;
    }

    /** The roles held, by profile name; throws when the question was not answered. */
    Map<String, List<String>> ask(String tenant, String app, String person) throws Exception {
        String payload = question(tenant, app, person);
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
        return parse(response.body());
    }
}
