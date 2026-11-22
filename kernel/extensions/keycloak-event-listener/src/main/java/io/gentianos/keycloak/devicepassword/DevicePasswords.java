/*
 * Copyright The Gentian OS Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * SPDX-License-Identifier: MPL-2.0
 */
package io.gentianos.keycloak.devicepassword;

import java.io.IOException;
import java.security.SecureRandom;
import java.util.List;
import java.util.Objects;
import java.util.Set;
import java.util.stream.Collectors;
import org.keycloak.common.util.Time;
import org.keycloak.credential.CredentialModel;
import org.keycloak.models.UserModel;
import org.keycloak.util.JsonSerialization;

/**
 * A person's device passwords, kept as credentials of the person in the
 * realm, beside their password and second factor.
 *
 * <p>The admin API lists them ({@code GET users/<id>/credentials}, which
 * never returns the secret data) and removes one
 * ({@code DELETE users/<id>/credentials/<id>}). Only creating one is this
 * extension's: Keycloak's API cannot create a credential of a type of its
 * own.
 */
public final class DevicePasswords {

    private static final SecureRandom RANDOM = new SecureRandom();

    /** Written at most this often per password: the door asks every few minutes. */
    static final long LAST_USED_GRAIN_MS = 60_000;

    private DevicePasswords() {
    }

    /** What is kept beside the label, readable by the admin API. */
    public static class Data {
        public String ref;
        public List<String> ways;
        public Long lastUsedAt;
    }

    /** What is kept as secret data, never returned by any API. */
    public static class Secret {
        public String digest;
    }

    /** A password just made, as it is shown once. */
    public record Created(String id, String name, List<String> ways, String password, long createdAt) {
    }

    /** One of the person's device passwords, read back. */
    public record Held(CredentialModel model, Data data, Secret secret) {
    }

    public static List<Held> of(UserModel user) {
        return user.credentialManager().getStoredCredentialsByTypeStream(DevicePassword.TYPE)
            .map(DevicePasswords::read)
            .filter(Objects::nonNull)
            .collect(Collectors.toList());
    }

    static Held read(CredentialModel m) {
        try {
            Data d = JsonSerialization.readValue(m.getCredentialData(), Data.class);
            Secret s = JsonSerialization.readValue(m.getSecretData(), Secret.class);
            if (d == null || d.ref == null || s == null || s.digest == null) {
                return null;
            }
            return new Held(m, d, s);
        } catch (IOException | RuntimeException e) {
            return null;
        }
    }

    /**
     * Makes a device password for a person. The caller has checked the name
     * and the ways; the count and a name the person already uses are checked
     * here, against what the person holds.
     */
    public static Created create(UserModel user, String name, List<String> ways) {
        List<Held> held = of(user);
        if (held.size() >= DevicePassword.MAX_PER_PERSON) {
            throw new IllegalArgumentException(
                "a person holds at most " + DevicePassword.MAX_PER_PERSON + " device passwords");
        }
        String label = name.strip();
        if (held.stream().anyMatch(h -> label.equals(h.model().getUserLabel()))) {
            throw new IllegalArgumentException("there is a device password named " + label + " already");
        }
        Set<String> refs = held.stream().map(h -> h.data().ref).collect(Collectors.toSet());
        DevicePassword.Issued issued = DevicePassword.generate(RANDOM);
        while (refs.contains(issued.ref())) {
            issued = new DevicePassword.Issued(DevicePassword.drawRef(RANDOM), issued.secret());
        }

        Data data = new Data();
        data.ref = issued.ref();
        data.ways = List.copyOf(ways);
        Secret secret = new Secret();
        secret.digest = DevicePassword.digest(user.getId(), issued.ref(), issued.secret());

        long now = Time.currentTimeMillis();
        CredentialModel m = new CredentialModel();
        m.setType(DevicePassword.TYPE);
        m.setUserLabel(label);
        m.setCreatedDate(now);
        try {
            m.setCredentialData(JsonSerialization.writeValueAsString(data));
            m.setSecretData(JsonSerialization.writeValueAsString(secret));
        } catch (IOException e) {
            throw new IllegalStateException(e);
        }
        CredentialModel stored = user.credentialManager().createStoredCredential(m);
        return new Created(stored.getId(), label, data.ways, issued.formatted(), now);
    }

    /** The password a presented one is, if it is one of the person's. */
    public static Held find(UserModel user, DevicePassword.Presented p) {
        for (Held h : of(user)) {
            if (h.data().ref.equals(p.ref())) {
                return DevicePassword.matches(user.getId(), p, h.secret().digest) ? h : null;
            }
        }
        return null;
    }

    /** Records a use, unless one was recorded within the last minute. */
    public static void markUsed(UserModel user, Held h, long now) {
        Long last = h.data().lastUsedAt;
        if (last != null && now - last < LAST_USED_GRAIN_MS) {
            return;
        }
        h.data().lastUsedAt = now;
        try {
            h.model().setCredentialData(JsonSerialization.writeValueAsString(h.data()));
        } catch (IOException e) {
            return;
        }
        user.credentialManager().updateStoredCredential(h.model());
    }

    /** Removes every device password of a person. Returns how many. */
    public static int removeAll(UserModel user) {
        List<String> ids = user.credentialManager().getStoredCredentialsByTypeStream(DevicePassword.TYPE)
            .map(CredentialModel::getId)
            .collect(Collectors.toList());
        ids.forEach(id -> user.credentialManager().removeStoredCredentialById(id));
        return ids.size();
    }
}
