/*
 * Copyright 2026 Gentian Technologies.
 * Licensed under the Apache License, Version 2.0.
 */
package io.gentianos.keycloak.events;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.security.PrivateKey;
import java.security.Signature;
import java.time.Duration;
import java.util.Base64;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;

import org.jboss.logging.Logger;

/**
 * Delivers events off the request thread, in order, signed.
 *
 * A login must not wait on the director, and must not fail because the director
 * is away. So delivery is one background thread with a bounded queue: events
 * are retried for about a minute and then given up on, loudly. Giving up is
 * safe to the extent that the next event for the same user carries their whole
 * state again, and the director's periodic comparison with Keycloak catches
 * what no later event does.
 */
final class Sender {

    private static final Logger LOG = Logger.getLogger(Sender.class);
    private static final int QUEUE = 10_000;
    private static final long[] BACKOFF_MS = {0, 500, 2_000, 8_000, 20_000, 30_000};

    private final URI url;
    private final String keyId;
    private final PrivateKey key;
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(5)).build();
    private final ThreadPoolExecutor worker = new ThreadPoolExecutor(1, 1, 0, TimeUnit.SECONDS,
        new LinkedBlockingQueue<>(QUEUE), runnable -> {
            Thread thread = new Thread(runnable, "gentian-director-events");
            thread.setDaemon(true);
            return thread;
        });

    Sender(String url, String keyId, PrivateKey key) {
        this.url = URI.create(url);
        this.keyId = keyId;
        this.key = key;
    }

    void enqueue(String payload) {
        try {
            worker.execute(() -> deliver(payload));
        } catch (RejectedExecutionException full) {
            LOG.errorf("membership event dropped: %d events are already waiting for the director", QUEUE);
        }
    }

    private void deliver(String payload) {
        for (int attempt = 0; attempt < BACKOFF_MS.length; attempt++) {
            try {
                Thread.sleep(BACKOFF_MS[attempt]);
                int status = post(payload);
                if (status / 100 == 2) {
                    return;
                }
                if (status / 100 == 4) {
                    // Refused, not unavailable: the same bytes will be refused again.
                    LOG.errorf("director refused a membership event with status %d; check the signing key and clock", status);
                    return;
                }
                LOG.warnf("director answered %d to a membership event (attempt %d)", status, attempt + 1);
            } catch (InterruptedException stop) {
                Thread.currentThread().interrupt();
                return;
            } catch (Exception e) {
                LOG.warnf("membership event not delivered (attempt %d): %s", attempt + 1, e.getMessage());
            }
        }
        LOG.error("membership event given up on; the director's comparison with Keycloak will have to find it");
    }

    /** Signs "<unix seconds>.<body>" afresh for every attempt: the receiver bounds how old a signature may be. */
    private int post(String payload) throws Exception {
        String timestamp = Long.toString(System.currentTimeMillis() / 1000);
        Signature signer = Signature.getInstance("Ed25519");
        signer.initSign(key);
        signer.update((timestamp + "." + payload).getBytes(StandardCharsets.UTF_8));
        String signature = Base64.getEncoder().encodeToString(signer.sign());

        HttpRequest request = HttpRequest.newBuilder(url)
            .timeout(Duration.ofSeconds(10))
            .header("Content-Type", "application/json")
            .header("X-Gentian-Signature", "keyid=" + keyId + ",t=" + timestamp + ",sig=" + signature)
            .POST(HttpRequest.BodyPublishers.ofString(payload, StandardCharsets.UTF_8))
            .build();
        return http.send(request, HttpResponse.BodyHandlers.discarding()).statusCode();
    }

    void close() {
        worker.shutdownNow();
    }
}
