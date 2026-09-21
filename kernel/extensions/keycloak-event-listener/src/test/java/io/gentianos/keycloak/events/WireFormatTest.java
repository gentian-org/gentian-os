/*
 * Copyright 2026 Gentian Technologies.
 * Licensed under the Apache License, Version 2.0.
 */
package io.gentianos.keycloak.events;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.Signature;
import java.util.Base64;
import java.util.List;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/** What leaves Keycloak is what the director verifies: "keyid=,t=,sig=" over "<t>.<body>". */
class WireFormatTest {

    @Test
    void aGroupNameCannotBreakOutOfItsString() {
        String json = Payload.userMemberships("demo", "u-1", List.of("a\"],\"realm\":\"kernel", "b\\", "c\nd"));
        assertTrue(json.contains("\"groups\":[\"a\\\"],\\\"realm\\\":\\\"kernel\",\"b\\\\\",\"c\\nd\"]"), json);
        assertTrue(json.startsWith("{\"id\":\""), json);
        assertTrue(json.contains("\"realm\":\"demo\",\"type\":\"user.memberships\",\"user\":\"u-1\""), json);
    }

    @Test
    void theKeyIsReadFromAPkcs8PemFile(@TempDir Path dir) throws Exception {
        KeyPair pair = KeyPairGenerator.getInstance("Ed25519").generateKeyPair();
        Path file = dir.resolve("key.pem");
        Files.writeString(file, "-----BEGIN PRIVATE KEY-----\n"
            + Base64.getMimeEncoder(64, "\n".getBytes()).encodeToString(pair.getPrivate().getEncoded())
            + "\n-----END PRIVATE KEY-----\n");
        assertEquals(pair.getPrivate(), DirectorEventListenerProviderFactory.readKey(file));
    }

    @Test
    void anEventIsSignedAndRetriedUntilTheDirectorTakesIt() throws Exception {
        KeyPair pair = KeyPairGenerator.getInstance("Ed25519").generateKeyPair();
        BlockingQueue<String[]> received = new ArrayBlockingQueue<>(4);
        AtomicInteger calls = new AtomicInteger();
        HttpServer director = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        director.createContext("/v1/events/keycloak", exchange -> {
            String body = new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
            if (calls.incrementAndGet() == 1) {
                exchange.sendResponseHeaders(503, -1); // away the first time
            } else {
                received.add(new String[] {exchange.getRequestHeaders().getFirst("X-Gentian-Signature"), body});
                exchange.sendResponseHeaders(204, -1);
            }
            exchange.close();
        });
        director.start();
        Sender sender = new Sender("http://127.0.0.1:" + director.getAddress().getPort() + "/v1/events/keycloak",
            "k1", pair.getPrivate());
        try {
            String payload = Payload.userDeleted("demo", "u-1");
            sender.enqueue(payload);
            String[] got = received.poll(10, TimeUnit.SECONDS);
            assertTrue(got != null, "the director never received the event");
            assertEquals(payload, got[1]);

            String[] fields = got[0].split(",", 3);
            assertEquals("keyid=k1", fields[0]);
            String timestamp = fields[1].substring("t=".length());
            byte[] signature = Base64.getDecoder().decode(fields[2].substring("sig=".length()));
            Signature verifier = Signature.getInstance("Ed25519");
            verifier.initVerify(pair.getPublic());
            verifier.update((timestamp + "." + payload).getBytes(StandardCharsets.UTF_8));
            assertTrue(verifier.verify(signature));
            assertTrue(Math.abs(System.currentTimeMillis() / 1000 - Long.parseLong(timestamp)) < 30);
        } finally {
            sender.close();
            director.stop(0);
        }
    }
}
