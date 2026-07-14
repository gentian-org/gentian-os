# Choosing an Official App Scaffold for Gentian OS: Maintained Full-Stack Templates Evaluated

## TL;DR
- **The two strongest candidates are the official `fastapi/full-stack-fastapi-template` (FastAPI + React + SQLModel + Postgres, ~43.4k stars, maintained by the FastAPI org) and `cookiecutter/cookiecutter-django` (Django + Postgres, ~13.5k stars, REVSYS/Jeff Triplett–backed, CalVer-released as recently as May 2026).** For a server-rendered, AI-agent-friendly path, **SaaS Pegasus** (commercial Django + HTMX/React) is the best "everything assembled" option but is paid and closed-source.
- **No mainstream scaffold ships Kubernetes manifests or a Helm chart out of the box** — every candidate stops at Docker Compose (or Vercel/Heroku). Whichever scaffold Gentian OS adopts, the platform team will have to author the Helm-chart packaging layer itself; this is a universal gap, not a differentiator.
- **There is no true "T3 stack for Python."** The closest philosophical analogs are the FastAPI template (typed, opinionated, React) and Django-as-a-whole (batteries included). Django's "batteries-included" model is genuinely resurgent in the AI-agent era because it minimizes decisions and produces highly conventional, LLM-predictable code.

## Key Findings

**Best overall fit for an AI-agent-driven, Helm-packaged app store:** A two-track recommendation. Track A (API + SPA): the official FastAPI full-stack template. Track B (server-rendered monolith, single container): Cookiecutter Django, optionally with the HTMX/Tailwind path. Both are typed, conventional, actively maintained, and institutionally backed — the qualities that matter most for both human teams and coding agents.

**Maintenance reality check (verified ~June 3, 2026):**
- `fastapi/full-stack-fastapi-template`: ~43.4k stars; release 0.10.0 dated 2026-01-23 per the official SourceForge mirror ("Last Update: 2026-01-23"); the template ships a React 19 + TypeScript frontend with TanStack Router/Query plus Biome, Zod, Ruff, and Playwright E2E. Maintained by the FastAPI organization (Sebastián Ramírez / tiangolo and team, notably @alejsdev and @estebanx64).
- `cookiecutter/cookiecutter-django`: ~13.5k stars; CalVer releases, most recent around 2026.05.18. Per Django Packages, verbatim: "© 2010-2026 Contributors, 2010-2021 funded by Two Scoops Press… Originally developed by Daniel Roy Greenfeld & Audrey Roy Greenfeld. Currently maintained by Jeff Triplett and development sponsored by REVSYS."
- `reflex-dev/reflex`: ~28.3k stars; latest release v0.8.28 on Mar 16, 2026; venture-backed company (YC, founded 2023 by Alek Petuskey and Nikhil Rao, ~10 employees, San Francisco).
- `t3-oss/create-t3-app`: ~28.6k stars; latest release Nov 5, 2025; maintained by Theo Browne, Julius Marminge and the T3-OSS community.
- `vintasoftware/nextjs-fastapi-template`: ~290–313 stars; latest release 0.0.8 on Dec 17, 2025; maintained by Vinta Software (a consultancy).
- `falcopackages/falco` (Django CLI/starter): single-maintainer project by Tobi DEGNON (PSF Fellow) — a red flag on bus-factor grounds.

**Kubernetes/Helm:** Out-of-box support is absent across the board. FastAPI template and Cookiecutter Django are the most production-hardened (both ship Docker + Traefik + LetsEncrypt), making them the easiest to wrap in a Helm chart. There is a long-standing community feature request for a Helm option in Cookiecutter Django that has not been implemented.

**AI-native scaffolds are an emerging but mostly commercial niche.** OpenAI released AGENTS.md in August 2025 and on Dec 9, 2025 donated it to the Linux Foundation's newly-formed Agentic AI Foundation (AAIF), alongside Anthropic's MCP and Block's goose; per OpenAI it has been "adopted by more than 60,000 open-source projects and agent frameworks including Amp, Codex, Cursor, Devin, Factory, Gemini CLI, GitHub Copilot, Jules and VS Code among others." SaaS Pegasus ships Cursor rules + CLAUDE.md + MCP config as of its 2025.4 version. Cookiecutter Django has adopted an AGENTS.md, but only for maintaining the template repo itself — it is not generated into downstream projects.

## Details

### 1. Django-based stacks
**Cookiecutter Django** is the flagship. It bundles a customizable users app, django-allauth authentication, PostgreSQL (versions 14–18), Docker Compose for dev and prod (Traefik + LetsEncrypt), Celery + Flower, Anymail email, optional Django REST Framework, optional async, and a choice of frontend pipeline (None / Django Compressor / Gulp / Webpack). It is secure-by-default and uses only maintained third-party libraries. It does **not** bundle a separate SPA frontend by default — its "frontend" is server-rendered Django templates with an optional asset pipeline. Maintenance is excellent (CalVer releases roughly monthly; ~13.5k stars; REVSYS-sponsored). Weakness for Gentian OS: no K8s/Helm, and the default is not a modern JS frontend.

Forks bundling Django + a JS SPA exist (e.g., `dmkibuka/cookiecutter-django-reactjs`, various Django+Vite+Inertia templates supporting Vue/React/Svelte) but are low-star, often stale, and one-person efforts — not safe institutional bases.

**Django + Svelte/SvelteKit:** Real integrations exist (django-vite, Inertia.js adapters, `Bishwas-py/django-svelte-template`, the `@friendofsvelte/django-kit` "DjangoKit") but all are small, individually maintained projects rather than institutionally backed scaffolds.

### 2. FastAPI-based stacks
**`fastapi/full-stack-fastapi-template`** is the strongest typed-API candidate. It bundles FastAPI, SQLModel (ORM), Pydantic, PostgreSQL, a React 19 + TypeScript + Vite frontend using TanStack Query and TanStack Router, JWT auth with secure password hashing (pwdlib/Argon2), Alembic migrations, Pytest with ≥90% coverage targets, Docker Compose, Traefik (automatic HTTPS), GitHub Actions CI/CD, Biome/Zod/Ruff for quality, Playwright for E2E, and auto-generated frontend API clients from the OpenAPI schema. It is generated via Copier. It recently migrated to `uv` workspaces, Bun, and a Node monorepo. ~43.4k stars; release 0.10.0 in Jan 2026. This is the most "T3-like" Python option: opinionated, typed end-to-end, single coherent template.

**`vintasoftware/nextjs-fastapi-template`** (Next.js + FastAPI + Zod + fastapi-users + Shadcn/ui + uv + Docker + Vercel) is modern and fully async but small (~300 stars) and consultancy-maintained — a credible reference, not a safe institutional default at this adoption level.

### 3. Python + HTMX stacks
The HTMX-on-Django world is vibrant but fragmented. **Cookiecutter Django** can be configured toward HTMX; community forks like `Alurith/django-cookiecutter` (Postgres + HTMX + Tailwind) and `imAsparky/django-cookiecutter` (django-tailwind by default, HTMX helpers, CI/CD) exist but are small. **Falco** (Tobi-De / falcopackages) is the most thoughtfully designed modern Django + HTMX + Tailwind + uv + Docker (s6-overlay single-container) starter and CRUD generator, but it is essentially a one-person project. **SaaS Pegasus** is the most complete HTMX option (see below).

### 4. SvelteKit-native full-stack
There is **no single widely adopted, institutionally backed SvelteKit + Drizzle + Postgres scaffold.** The space is a long tail of individual templates (`qwacko/sveltekit-lucia-starter`, `delay/sveltekit-auth`, many others), most small and personal. Critically, **Lucia — the auth library most of these starters depend on — has been deprecated.** Lucia v3 was deprecated by March 2025 by its maintainer (pilcrowonpaper): "It's official - I'll be deprecating Lucia v3 by March 2025… Lucia is now a learning resource on implementing auth from scratch" (lucia-auth/lucia GitHub Discussion #1714), with adapters deprecated at the end of 2024. The ecosystem is migrating to Better Auth or Auth.js. This makes most existing SvelteKit auth starters a maintenance liability. SvelteKit + Drizzle + Better Auth is a sound modern combination, but you would be assembling it, not adopting a curated scaffold.

### 5. T3 Stack and equivalents
**T3 (`create-t3-app`)** is real and widely adopted (~28.6k stars; maintained by Theo Browne and Julius Marminge; tracks every Next.js/tRPC release). By 2025–2026 the "Modern T3" default shifted from Prisma to Drizzle, NextAuth became Auth.js v5, and Server Actions encroached on tRPC's role. Zoom adopted T3 for its SDK reference apps. But create-t3-app is explicitly **not** all-inclusive — it deliberately leaves out deployment and many libraries.

**There is no direct Python equivalent of T3's "you don't have to make decisions" CLI.** The FastAPI full-stack template is the closest in spirit (typed, opinionated, batteries included) but is a single template rather than a modular generator. Django-as-a-whole is the other answer.

### 6. Django as a complete answer
Django without a separate frontend framework is a genuinely viable default. Its built-in admin, ORM, migrations, auth, forms, sessions, and security defaults (CSRF, XSS protections) remove most of the decisions a team would otherwise make — which is exactly what helps coding agents produce correct, conventional code. The Django community is actively discussing AI's impact, "batteries included" in 2025, and a REST story; Django co-creator Simon Willison is a leading AI commentator, and Will Vincent's DjangoCon US 2025 talk centered on Django-for-AI. Practically, Django + HTMX is widely argued to be the fastest path to a production SaaS for Python teams in 2026 because it keeps routing, ORM, auth, admin, and templates in one server-rendered loop — ideal for a single-container Kubernetes deployment.

### 7. Emerging "AI-native" scaffolds
The convention to align with is **AGENTS.md** — a vendor-neutral Markdown "README for agents" now read natively by Claude Code, Codex, Cursor, GitHub Copilot, Gemini CLI, and many others, and stewarded by the Linux Foundation's Agentic AI Foundation since Dec 2025. Concrete scaffolds shipping agent rules:
- **SaaS Pegasus** (commercial, Cory Zue): ships Cursor rules, a consolidated CLAUDE.md, `.mcp.json` MCP setup, an optional GitHub `@claude` workflow, JetBrains Junie guidelines, and llms.txt files as of version 2025.4. The most mature AI-native option, but paid and closed-source.
- **DjangoBlaze** (commercial, solo maintainer): markets itself as "the only Django boilerplate designed for AI-assisted development," shipping `.cursorrules`, CLAUDE.md, AI docs and prompts. Cheap but single-maintainer.
- **Cookiecutter Django**: contains an AGENTS.md, but it guides agents working on the template repo itself — it is not emitted into generated projects.
- **Reflex**: explicitly markets itself as "optimized to build apps that can be used by humans and AI-agents alike," offers an MCP + Skills onboarding, and publishes machine-readable docs (llms.txt). Pure-Python full-stack (compiles to Next.js/React under the hood; backend FastAPI; SQLModel/SQLAlchemy + Alembic migrations). Reflex's Y Combinator profile states it "has powered over 1 million applications, earned 28,000+ GitHub stars, and is used by 30% of Fortune 500 companies for internal tools and data-driven applications" (its own site claims 40% of the Fortune 500). Caveat: heavier "magic" (Python compiled to React) cuts against the "minimal magic, clear conventions" criterion, and self-hosting is Docker-based with no Helm chart.

## Recommendations

**Adopt a two-track official scaffold, and own the Helm layer yourself.**

1. **Primary (Track A — typed API + SPA): `fastapi/full-stack-fastapi-template`.** Fork it into the Gentian OS org, pin versions, and add: (a) a Helm chart wrapping the backend, frontend, and a Postgres dependency (or external managed Postgres), (b) an AGENTS.md / CLAUDE.md at repo root encoding Gentian conventions, and (c) a values.yaml schema matching your app-store packaging. Rationale: end-to-end typing, OpenAPI-generated clients, strong tests, and the largest institutional backing of any candidate — the best substrate for AI-agent codegen.

2. **Secondary (Track B — server-rendered single container): `cookiecutter/cookiecutter-django`**, configured with the HTMX/Tailwind path (or a documented Pegasus license for teams that want billing/teams pre-built). Rationale: a single container, minimal moving parts, Django's batteries reduce agent decision-space, and it is the most production-hardened free Django scaffold. Add the same Helm + AGENTS.md layer.

3. **Build the missing piece once, centrally:** Since no scaffold ships Helm, create a Gentian OS "app-chart" Helm library chart and a cookiecutter/copier wrapper that injects it into either track. This converts the universal gap into a platform advantage — every cloned app is Helm-ready by construction.

**Benchmarks that would change these recommendations:**
- If a SvelteKit team is a hard requirement, wait for or vet a Better Auth–based (not Lucia) Drizzle + Postgres scaffold with real institutional backing before standardizing; today none qualifies.
- If "pure Python, no JS" and rapid internal tools dominate, re-evaluate **Reflex** despite its magic — its explicit AI-agent optimization and single-language model may outweigh the abstraction cost.
- If the FastAPI template's React SPA proves too heavy for simple apps, make Track B (Django/HTMX) the default and reserve Track A for data-heavy or API-first apps.

## Caveats
- **All star counts and dates were captured around June 3, 2026** and drift over time; treat them as maintenance proxies, not precise figures. Reported numbers from different pages varied slightly (e.g., FastAPI template 38.6k–43.4k across sources; the higher live figure is more current).
- **"AI handles Django/FastAPI well" is an inference** from the conventional, well-documented nature of these frameworks and community reports, not a controlled benchmark. Several cited "2026" trend articles use forward-looking or marketing language; AGENTS.md adoption statistics come from OpenAI/Linux Foundation announcements and secondary sources and should be treated as directional.
- **Commercial options (SaaS Pegasus, DjangoBlaze) lack public GitHub metrics** because they are closed-source; their maintenance and adoption claims are self-reported. Reflex's Fortune 500 figures are likewise self-reported and stated inconsistently (30% vs 40%) across its own materials.
- **Lucia's deprecation** is the single most important "red flag" affecting the SvelteKit ecosystem; any SvelteKit decision must avoid Lucia-based starters.
- The **K8s/Helm gap is universal** across the five mainstream scaffolds examined; this is the most consequential finding for a Helm-native platform and is not expected to change without platform-side work.