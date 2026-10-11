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
