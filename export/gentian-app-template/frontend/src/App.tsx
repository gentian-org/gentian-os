import { useEffect, useState } from "react";

type Health = { status: string };

export default function App() {
  const [health, setHealth] = useState<Health | null>(null);

  useEffect(() => {
    fetch("/healthz")
      .then((r) => r.json())
      .then(setHealth)
      .catch(() => setHealth({ status: "error" }));
  }, []);

  return (
    <main className="mx-auto flex min-h-screen max-w-3xl flex-col gap-6 p-8">
      <header>
        <p className="text-sm font-medium uppercase tracking-wide text-indigo-600">
          Gentian App Template
        </p>
        <h1 className="mt-2 text-3xl font-semibold">Your application</h1>
        <p className="mt-2 text-slate-600">
          FastAPI + React starter. Replace this page with your product UI.
        </p>
      </header>
      <section className="rounded-xl border border-slate-200 bg-white p-6 shadow-sm">
        <h2 className="font-medium">API health</h2>
        <p className="mt-2 text-slate-600">
          {health ? `Backend: ${health.status}` : "Checking…"}
        </p>
      </section>
    </main>
  );
}
