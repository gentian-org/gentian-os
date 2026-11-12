# cache chart

| Field | Value |
|---|---|
| Purpose | The shared kernel cache: Valkey, in a StatefulSet of one |
| Upstream | None: written here, around the official image |
| Image | `docker.io/valkey/valkey`, pinned by digest in `values.yaml` |
| Package repo | `charts/infra/packages/`, regenerate with `scripts/tools/publish-infra-charts.sh` |
| License | Valkey is BSD-3-Clause |

The names are the cache's function, not its engine. The engine is the
`image` value alone: the templates start the server and the probes run the
client by the names the Valkey and the Redis images both ship
(`redis-server`, `redis-cli`), so another server that speaks the Redis
protocol is a values change. Apps speak that protocol to it; `redis` is the
name of the cache engine in a profile.

Helm values and ESO secrets are in `kernel/services/infra-cache/`.
