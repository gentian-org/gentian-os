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

import java.net.URI;
import java.net.URISyntaxException;
import java.util.List;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import java.util.regex.PatternSyntaxException;

/**
 * One authorization-request parameter of a client, what it may be, and the
 * one redirect address a code for a request carrying it may be delivered to,
 * computed from it.
 *
 * <p>A client declares it in four attributes, which the platform's app
 * Composition writes from the profile ({@code requires.services.identity.oidc.
 * requestParameter}):
 *
 * <pre>
 * gentianos.io/request-bound-redirect           "true"
 * gentianos.io/request-parameter                tenant_url
 * gentianos.io/request-parameter-pattern        https://(?&lt;host&gt;...)
 * gentianos.io/request-parameter-redirect-uri   https://store.${host}/oauth/callback
 * </pre>
 *
 * <p>{@link RequestBoundRedirectExecutor} refuses a request whose
 * {@code redirect_uri} is not {@link #redirectFor} its parameter, and
 * {@link RequestParameterMapper} puts the parameter in a token only when the
 * code was exchanged for that same address. Both read this class and nothing
 * else, so the two cannot disagree about what a value allows.
 *
 * <p>Everything here fails closed. A client that carries the flag with an
 * attribute missing or unreadable is {@link #INVALID}: its every request is
 * refused and none of its tokens carries the value. A value is matched as a
 * whole, against at most {@link #MAX_VALUE} characters and with a bound on
 * the work ({@link BoundedChars}), because the pattern is the profile's.
 */
public final class RequestBinding {

    public static final String FLAG = "gentianos.io/request-bound-redirect";
    public static final String PARAMETER = "gentianos.io/request-parameter";
    public static final String PATTERN = "gentianos.io/request-parameter-pattern";
    public static final String REDIRECT_URI = "gentianos.io/request-parameter-redirect-uri";

    /** The longest value matched at all. */
    public static final int MAX_VALUE = 512;

    /** A client that carries the flag and cannot say what it binds. */
    public static final RequestBinding INVALID = new RequestBinding(null, null, null);

    private static final Pattern GROUP = Pattern.compile("\\$\\{([A-Za-z][A-Za-z0-9]*)}");

    private final String parameter;
    private final Pattern pattern;
    private final String redirectTemplate;

    private RequestBinding(String parameter, Pattern pattern, String redirectTemplate) {
        this.parameter = parameter;
        this.pattern = pattern;
        this.redirectTemplate = redirectTemplate;
    }

    /**
     * The binding a client's attributes declare: {@code null} for a client
     * that does not carry the flag, {@link #INVALID} for one that carries it
     * and cannot be read.
     */
    public static RequestBinding of(Map<String, String> attributes) {
        if (attributes == null || !"true".equals(attributes.get(FLAG))) {
            return null;
        }
        String parameter = attributes.get(PARAMETER);
        String pattern = attributes.get(PATTERN);
        String redirect = attributes.get(REDIRECT_URI);
        if (isBlank(parameter) || isBlank(pattern) || isBlank(redirect)) {
            return INVALID;
        }
        if (!redirect.startsWith("https://") || !GROUP.matcher(redirect).find()) {
            return INVALID;
        }
        try {
            return new RequestBinding(parameter, Pattern.compile(pattern), redirect);
        } catch (PatternSyntaxException e) {
            return INVALID;
        }
    }

    public boolean isValid() {
        return this != INVALID;
    }

    /** The parameter's name, as the app sends it. */
    public String parameter() {
        return parameter;
    }

    /**
     * The one redirect address a request carrying this value may name, or
     * {@code null} when the value allows none: absent, too long, not matched
     * as a whole, matched with too much work, naming a group the pattern
     * does not have, or computing an address that is not plain https.
     */
    public String redirectFor(String value) {
        if (!isValid() || value == null || value.isEmpty() || value.length() > MAX_VALUE) {
            return null;
        }
        Matcher m = pattern.matcher(new BoundedChars(value));
        try {
            if (!m.matches()) {
                return null;
            }
        } catch (BoundedChars.TooMuchWork e) {
            return null;
        }
        StringBuilder out = new StringBuilder();
        Matcher g = GROUP.matcher(redirectTemplate);
        int at = 0;
        while (g.find()) {
            out.append(redirectTemplate, at, g.start());
            String part;
            try {
                part = m.group(g.group(1));
            } catch (IllegalArgumentException e) {
                return null;
            }
            if (part == null || part.isEmpty()) {
                return null;
            }
            out.append(part);
            at = g.end();
        }
        out.append(redirectTemplate.substring(at));
        String computed = out.toString();
        return isPlainHttps(computed) ? computed : null;
    }

    /** https, a host, no user, port, query, fragment or wildcard. */
    static boolean isPlainHttps(String address) {
        if (address.contains("*") || address.contains("$") || address.contains("\\")) {
            return false;
        }
        try {
            URI u = new URI(address);
            return "https".equals(u.getScheme())
                    && u.getHost() != null
                    && u.getRawUserInfo() == null
                    && u.getPort() == -1
                    && u.getRawQuery() == null
                    && u.getRawFragment() == null;
        } catch (URISyntaxException e) {
            return false;
        }
    }

    /** The one value of a parameter a request carries, or {@code null} for none or more than one. */
    public static String single(List<String> values) {
        if (values == null || values.size() != 1) {
            return null;
        }
        return values.get(0);
    }

    private static boolean isBlank(String s) {
        return s == null || s.isBlank();
    }
}
