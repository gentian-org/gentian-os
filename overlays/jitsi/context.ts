// -----------------------------------------------------------------------------
// SPDX-FileCopyrightText: 2024 Zentrum für Digitale Souveränität der Öffentlichen Verwaltung (ZenDiS) GmbH
// SPDX-FileCopyrightText: 2023 Bundesministerium des Innern und für Heimat, PG ZenDiS "Projektgruppe für Aufbau ZenDiS"
// SPDX-License-Identifier: Apache-2.0
// Gentian: accept preferred_username when opendesk_username is absent (kernel IdP broker).
// -----------------------------------------------------------------------------

export function createContext(userInfo: Record<string, unknown>) {
  const username = userInfo.opendesk_username || userInfo.preferred_username;
  if (!username) throw new Error("no username");

  const context = {
    user: {
      id: userInfo.sub,
      name: userInfo.name || username,
      email: userInfo.email || "",
      lobby_bypass: true,
    },
  };
  return context;
}
