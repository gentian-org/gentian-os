/*
 * Copyright 2026 Gentian Technologies.
 * Licensed under the Apache License, Version 2.0.
 */
package io.gentianos.keycloak.events;

import org.keycloak.models.AbstractKeycloakTransaction;

/** Runs an action if, and only if, the surrounding transaction commits. */
final class AfterCommit extends AbstractKeycloakTransaction {

    private final Runnable action;

    AfterCommit(Runnable action) {
        this.action = action;
    }

    @Override
    protected void commitImpl() {
        action.run();
    }

    @Override
    protected void rollbackImpl() {
        // The change did not happen; there is nothing to report.
    }
}
