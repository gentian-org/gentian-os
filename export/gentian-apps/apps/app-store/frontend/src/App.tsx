import { useCallback, useEffect, useState } from "react";

type CatalogueApp = {
  name: string;
  displayName: string;
  description?: string;
  logo?: string;
  chartVersion: string;
  kernelRequirements: string[];
  installedCount: number;
};

type InstalledApp = {
  profile: string;
  source: string;
  ready?: boolean;
};

type CatalogueResponse = {
  apps: CatalogueApp[];
  catalogueRepo: string;
  catalogueBranch: string;
  lastUpdated?: string;
};

const API = "/api/v1";

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API}${path}`, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers || {}) },
  });
  if (!res.ok) {
    const body = await res.text();
    throw new Error(body || res.statusText);
  }
  return res.json() as Promise<T>;
}

export default function App() {
  const [catalogue, setCatalogue] = useState<CatalogueResponse | null>(null);
  const [installed, setInstalled] = useState<InstalledApp[]>([]);
  const [selected, setSelected] = useState<CatalogueApp | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setError(null);
    const [cat, inst] = await Promise.all([
      apiFetch<CatalogueResponse>("/catalogue/"),
      apiFetch<{ apps: InstalledApp[] }>("/tenant/apps/installed"),
    ]);
    setCatalogue(cat);
    setInstalled(inst.apps);
  }, []);

  useEffect(() => {
    refresh().catch((e: Error) => setError(e.message));
  }, [refresh]);

  const installedSet = new Set(installed.map((a) => a.profile));

  async function install(profile: string) {
    setBusy(profile);
    setError(null);
    try {
      await apiFetch(`/tenant/apps/${profile}/install`, { method: "POST" });
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Install failed");
    } finally {
      setBusy(null);
    }
  }

  async function uninstall(profile: string) {
    setBusy(profile);
    setError(null);
    try {
      await apiFetch(`/tenant/apps/${profile}`, { method: "DELETE" });
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Uninstall failed");
    } finally {
      setBusy(null);
    }
  }

  return (
    <main className="mx-auto min-h-screen max-w-6xl p-6 md:p-10">
      <header className="mb-8">
        <p className="text-sm font-semibold uppercase tracking-wide text-indigo-600">
          Gentian OS
        </p>
        <h1 className="mt-2 text-3xl font-bold text-slate-900">App Store</h1>
        <p className="mt-2 max-w-2xl text-slate-600">
          Browse available applications and install them for your tenant with one click.
        </p>
        {catalogue && (
          <p className="mt-3 text-xs text-slate-500">
            Catalogue: {catalogue.catalogueRepo} @ {catalogue.catalogueBranch}
          </p>
        )}
      </header>

      {error && (
        <div className="mb-6 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-red-800">
          {error}
        </div>
      )}

      <section className="mb-10">
        <h2 className="mb-4 text-lg font-semibold">Installed</h2>
        {installed.length === 0 ? (
          <p className="text-slate-500">No apps installed yet.</p>
        ) : (
          <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {installed.map((app) => (
              <li
                key={app.profile}
                className="flex items-center justify-between rounded-xl border border-slate-200 bg-white p-4 shadow-sm"
              >
                <div>
                  <p className="font-medium">{app.profile}</p>
                  <p className="text-xs text-slate-500">
                    {app.ready ? "Ready" : "Pending"} · {app.source}
                  </p>
                </div>
                <button
                  type="button"
                  disabled={busy === app.profile}
                  onClick={() => uninstall(app.profile)}
                  className="rounded-lg border border-slate-300 px-3 py-1.5 text-sm hover:bg-slate-50 disabled:opacity-50"
                >
                  Uninstall
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section>
        <h2 className="mb-4 text-lg font-semibold">Catalogue</h2>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {(catalogue?.apps || []).map((app) => (
            <article
              key={app.name}
              className="flex flex-col rounded-xl border border-slate-200 bg-white p-5 shadow-sm transition hover:border-indigo-200"
            >
              <div className="flex items-start gap-3">
                {app.logo ? (
                  <img src={app.logo} alt="" className="h-10 w-10 rounded-lg" />
                ) : (
                  <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-indigo-100 text-sm font-bold text-indigo-700">
                    {app.displayName.slice(0, 1)}
                  </div>
                )}
                <div className="min-w-0 flex-1">
                  <h3 className="truncate font-semibold">{app.displayName}</h3>
                  <p className="text-xs text-slate-500">v{app.chartVersion}</p>
                </div>
              </div>
              <p className="mt-3 line-clamp-3 flex-1 text-sm text-slate-600">
                {app.description || "No description."}
              </p>
              <div className="mt-3 flex flex-wrap gap-1">
                {app.kernelRequirements.map((req) => (
                  <span
                    key={req}
                    className="rounded-full bg-slate-100 px-2 py-0.5 text-xs text-slate-700"
                  >
                    {req}
                  </span>
                ))}
              </div>
              <div className="mt-4 flex gap-2">
                <button
                  type="button"
                  onClick={() => setSelected(app)}
                  className="rounded-lg border border-slate-300 px-3 py-1.5 text-sm hover:bg-slate-50"
                >
                  Details
                </button>
                {installedSet.has(app.name) ? (
                  <span className="flex items-center text-sm text-emerald-700">Installed</span>
                ) : (
                  <button
                    type="button"
                    disabled={busy === app.name}
                    onClick={() => install(app.name)}
                    className="rounded-lg bg-indigo-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:opacity-50"
                  >
                    Install
                  </button>
                )}
              </div>
            </article>
          ))}
        </div>
      </section>

      {selected && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
          <div className="max-h-[80vh] w-full max-w-lg overflow-auto rounded-2xl bg-white p-6 shadow-xl">
            <h3 className="text-xl font-semibold">{selected.displayName}</h3>
            <p className="mt-2 text-sm text-slate-600">{selected.description}</p>
            <p className="mt-4 text-sm">
              <span className="font-medium">Profile:</span> {selected.name}
            </p>
            <p className="text-sm">
              <span className="font-medium">Chart version:</span> {selected.chartVersion}
            </p>
            <p className="text-sm">
              <span className="font-medium">Cluster installs:</span> {selected.installedCount}{" "}
              tenants
            </p>
            <button
              type="button"
              onClick={() => setSelected(null)}
              className="mt-6 rounded-lg border border-slate-300 px-4 py-2 text-sm"
            >
              Close
            </button>
          </div>
        </div>
      )}
    </main>
  );
}
