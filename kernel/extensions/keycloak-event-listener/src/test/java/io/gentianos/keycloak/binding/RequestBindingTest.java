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

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

/**
 * What a bound request parameter allows, and the two checks that rest on it:
 * the executor's refusal and the mapper's claim.
 */
class RequestBindingTest {

    static final String HOST = "[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+";

    /** The attributes the app Composition writes for a store-like client. */
    static Map<String, String> attributes() {
        Map<String, String> a = new HashMap<>();
        a.put(RequestBinding.FLAG, "true");
        a.put(RequestBinding.PARAMETER, "tenant_url");
        a.put(RequestBinding.PATTERN, "https://(?<host>" + HOST + ")");
        a.put(RequestBinding.REDIRECT_URI, "https://store.${host}/oauth/callback");
        return a;
    }

    static RequestBinding binding() {
        return RequestBinding.of(attributes());
    }

    static Map<String, List<String>> sent(String tenantURL, String redirect) {
        Map<String, List<String>> p = new HashMap<>();
        p.put("client_id", List.of("gentian-app-store"));
        if (tenantURL != null) {
            p.put("tenant_url", List.of(tenantURL));
        }
        if (redirect != null) {
            p.put("redirect_uri", List.of(redirect));
        }
        return p;
    }

    @Test
    void theAddressIsComputedFromTheValue() {
        assertEquals("https://store.acme.example.com/oauth/callback", binding().redirectFor("https://acme.example.com"));
    }

    @Test
    void aValueTheBindingDoesNotAllowComputesNoAddress() {
        RequestBinding b = binding();
        for (String v : new String[] {
            null, "", "http://acme.example.com", "https://acme.example.com/", "https://acme.example.com/path",
            "https://ACME.example.com", "https://acme", "https://acme.example.com:8443", "https://evil.example?x=.acme.example",
            "https://acme.example.com#", " https://acme.example.com", "https://" + "a".repeat(600) + ".example",
        }) {
            assertNull(b.redirectFor(v), String.valueOf(v));
        }
    }

    @Test
    void aClientWithoutTheFlagDeclaresNothing() {
        Map<String, String> a = attributes();
        a.remove(RequestBinding.FLAG);
        assertNull(RequestBinding.of(a));
        a.put(RequestBinding.FLAG, "");
        assertNull(RequestBinding.of(a));
        assertNull(RequestBinding.of(null));
    }

    @Test
    void aClientWithTheFlagAndNothingReadableIsInvalidAndAllowsNothing() {
        for (String key : new String[] {RequestBinding.PARAMETER, RequestBinding.PATTERN, RequestBinding.REDIRECT_URI}) {
            Map<String, String> a = attributes();
            a.put(key, "");
            assertSame(RequestBinding.INVALID, RequestBinding.of(a), key);
        }
        Map<String, String> a = attributes();
        a.put(RequestBinding.PATTERN, "https://(?<host>[a-z");
        assertSame(RequestBinding.INVALID, RequestBinding.of(a));
        a = attributes();
        a.put(RequestBinding.REDIRECT_URI, "https://store.example/oauth/callback");
        assertSame(RequestBinding.INVALID, RequestBinding.of(a), "an address not computed from the value");
        a = attributes();
        a.put(RequestBinding.REDIRECT_URI, "http://store.${host}/oauth/callback");
        assertSame(RequestBinding.INVALID, RequestBinding.of(a));
        assertNull(RequestBinding.INVALID.redirectFor("https://acme.example.com"));
    }

    @Test
    void anAddressNamingAGroupThePatternLacksComputesNothing() {
        Map<String, String> a = attributes();
        a.put(RequestBinding.REDIRECT_URI, "https://store.${tenant}/oauth/callback");
        assertNull(RequestBinding.of(a).redirectFor("https://acme.example.com"));
    }

    @Test
    void aComputedAddressThatIsNotPlainHttpsIsNone() {
        Map<String, String> a = attributes();
        a.put(RequestBinding.PATTERN, "(?<host>.+)");
        RequestBinding b = RequestBinding.of(a);
        assertNotNull(b.redirectFor("acme.example.com"));
        for (String v : new String[] {"evil.example/x?y=", "evil.example#", "user@evil.example", "evil.example:444", "*.example", "a b"}) {
            assertNull(b.redirectFor(v), v);
        }
    }

    @Test
    void aPatternThatBacktracksWithoutEndIsStoppedAndAllowsNothing() {
        Map<String, String> a = attributes();
        a.put(RequestBinding.PATTERN, "(?<host>(a+)+)b");
        a.put(RequestBinding.REDIRECT_URI, "https://store.${host}/cb");
        long start = System.nanoTime();
        assertNull(RequestBinding.of(a).redirectFor("a".repeat(40) + "c"));
        assertTrue(System.nanoTime() - start < 5_000_000_000L, "the match was not stopped");
    }

    // ── the executor ─────────────────────────────────────────────────────

    @Test
    void theExecutorAdmitsTheOneAddressForTheValue() {
        assertNull(RequestBoundRedirectExecutor.sentRefusal(binding(),
                sent("https://acme.example.com", "https://store.acme.example.com/oauth/callback")));
        assertNull(RequestBoundRedirectExecutor.parsedRefusal(binding(),
                Map.of("tenant_url", "https://acme.example.com"),
                "https://store.acme.example.com/oauth/callback", "https://store.acme.example.com/oauth/callback"));
    }

    @Test
    void theExecutorRefusesEveryOtherRequest() {
        RequestBinding b = binding();
        String ok = "https://store.acme.example.com/oauth/callback";
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, sent(null, ok)), "no parameter");
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, sent("https://acme.example.com", null)), "no redirect_uri");
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, sent("https://other.example.com", ok)), "another tenant's address");
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, sent("https://acme.example.com", "https://evil.example/oauth/callback")), "an address elsewhere");
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, sent("https://acme.example.com", "https://mail.acme.example.com/oauth/callback")), "another app of the tenant");
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, sent("https://acme.example.com", ok + "/")), "not character for character");
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, sent("https://acme.example.com", ok + "?x=1")), "a query");

        Map<String, List<String>> twice = sent("https://acme.example.com", ok);
        twice.put("tenant_url", List.of("https://acme.example.com", "https://other.example.com"));
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, twice), "the parameter twice");
        twice = sent("https://acme.example.com", ok);
        twice.put("redirect_uri", List.of(ok, "https://evil.example/"));
        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, twice), "redirect_uri twice");

        for (String carrier : new String[] {"request", "request_uri"}) {
            Map<String, List<String>> p = sent("https://acme.example.com", ok);
            p.put(carrier, List.of("x"));
            assertNotNull(RequestBoundRedirectExecutor.sentRefusal(b, p), carrier);
        }

        assertNotNull(RequestBoundRedirectExecutor.sentRefusal(RequestBinding.INVALID, sent("https://acme.example.com", ok)), "an unreadable binding");
        assertNotNull(RequestBoundRedirectExecutor.parsedRefusal(b, Map.of("tenant_url", "https://acme.example.com"), ok, "https://store.acme.example.com/oauth/callback/"), "a redirect check that settled elsewhere");
        assertNotNull(RequestBoundRedirectExecutor.parsedRefusal(b, Map.of(), ok, ok), "a parsed request without the parameter");
    }

    @Test
    void aClientThatDeclaresNoneIsNotTheExecutorsToRefuse() {
        assertNull(RequestBoundRedirectExecutor.sentRefusal(null, sent(null, "https://anywhere.example/")));
        assertNull(RequestBoundRedirectExecutor.parsedRefusal(null, Map.of(), null, null));
    }

    // ── the mapper ───────────────────────────────────────────────────────

    @Test
    void theClaimIsTheValueAtTheExchangeOfACodeForItsAddress() {
        assertEquals("https://acme.example.com", RequestParameterMapper.claimValue(binding(),
                "https://acme.example.com", "authorization_code", "https://store.acme.example.com/oauth/callback"));
    }

    @Test
    void anyOtherTokenCarriesNoClaim() {
        RequestBinding b = binding();
        String ok = "https://store.acme.example.com/oauth/callback";
        // The session's note was overwritten by a later sign-in for another tenant.
        assertNull(RequestParameterMapper.claimValue(b, "https://other.example.com", "authorization_code", ok));
        assertNull(RequestParameterMapper.claimValue(b, "https://acme.example.com", "refresh_token", null));
        assertNull(RequestParameterMapper.claimValue(b, "https://acme.example.com", "refresh_token", ok));
        assertNull(RequestParameterMapper.claimValue(b, "https://acme.example.com", "urn:ietf:params:oauth:grant-type:token-exchange", ok));
        assertNull(RequestParameterMapper.claimValue(b, "https://acme.example.com", "authorization_code", null));
        assertNull(RequestParameterMapper.claimValue(b, null, "authorization_code", ok));
        assertNull(RequestParameterMapper.claimValue(b, "http://acme.example.com", "authorization_code", ok));
        assertFalse(RequestBinding.INVALID.isValid());
    }
}
