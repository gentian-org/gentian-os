---
audience: architect, operator
type: concept
---

# LLM Serving Integration Design

This document details the architecture for integrating Large Language Model (LLM) serving capabilities into Gentian OS.

## What runs

*   **The model gateway** is LiteLLM (`litellm-proxy`), with its own PostgreSQL
    (`litellm-db`) and Redis (`redis-llm`), in the namespace `system-llm`, which
    exists only where the Cluster claim sets `spec.llm.enabled`
    (`kernel/services/llm/manifests`, delivered by the `gentian-llm`
    ApplicationSet). On a cluster without GPUs the same chart runs a mock
    model server (`vllm-inference`).
*   **No request passes the edge.** The gateway has no public address unless the
    console is switched on (§5, "Operator access"). There is no token
    validation in front of the gateway and no per-model check in OpenFGA: an app presents its key to LiteLLM, and that is the
    whole check.
*   **A component gets access by declaring it** (`requires.services.llm` in its
    `ComponentProfile`). The operator then generates a key for that tenant and
    component, registers it at the gateway, writes the Secret
    `llm-credentials-<component>` (`OPENAI_API_BASE`, `OPENAI_API_BASE_URL`,
    `OPENAI_API_KEY`) in the tenant's namespace, and opens the component's
    network path to the gateway's port
    (`internal/controller/model_access_reconciler.go`, `internal/modelgateway`).
    A component that does not declare it gets none of these.
*   **One LiteLLM team per tenant**, created by the tenant reconciler
    (`internal/controller/litellm_team.go`). The operator sets no budget and no
    rate limit on a team or a key.
*   **A budget per person**, when the claim states one
    (`spec.llm.personBudget`: spend per period, requests and tokens per
    minute). The gateway holds every person it is told about to it: the
    desktop's assistant sends the person's subject as the request's `user`,
    and Open WebUI its own id of the person in `x-litellm-end-user-id`. A
    request over it is refused with 429. The budget is the gateway's, under
    the id `gentian-person`, written by a Job of the gateway's chart after
    every sync (`kernel/services/llm/manifests/templates/person-budget.yaml`).
    One budget holds for every person on the cluster. An app that names no person is held to nothing per person.
*   **The gateway's models are the Cluster claim's** (§5). The gateway's chart
    writes its configuration file from `spec.llm.instances` and
    `spec.llm.providers`, and the claim reaches the chart as a values file of
    the `gentian-llm` ApplicationSet (`kernel/appsets/raw/09c-llm.yaml`,
    `kernel/services/llm/manifests/templates/gateway-config.yaml`). Nothing
    registers a model through the gateway's API, and the gateway takes none
    from its database.
*   **The cluster's administrator changes them in the administration
    console** (§5, "Changing the models"), or with `kubectl gentian models`.
    Both ask the director, which commits the claim. The console flags a
    provider's model whose token is missing.
*   **Open WebUI comes with the models.** While `spec.llm.enabled` is true,
    the director keeps `open-webui-ce` in every user tenant's apps, for
    everyone, and removes it when the cluster stops serving models
    (`internal/director/api/platform_apps.go`). The profile says
    `placement: {tenants: users, while: llm}`, and a cluster catalog's index
    repeats it as `placedWith: llm`; the App Store does not offer it, and an
    uninstall by hand is refused.
*   **The vLLM instances.** Each entry of `spec.llm.instances` is a
    model of the gateway and a `vllm-<name>-inference` Deployment, Service,
    PVC and NetworkPolicy in `system-llm`, from the same chart (§6).

---

## 5. External Providers

A cluster does not have to serve its own weights. `spec.llm.providers` on the
Cluster claim declares external, OpenAI-compatible endpoints, and they route
through the same LiteLLM gateway as the vLLM instances — so the keys and
per-tenant teams apply to them unchanged. It is independent of
`gpuAcceleration`: a CPU-only cluster with no `instances` is the case these
exist for.

```yaml
spec:
  llm:
    enabled: true
    providers:
      - name: infomaniak
        displayName: Infomaniak AI Services
        apiBase: https://api.infomaniak.com/2/ai/<product-id>/openai/v1
        apiKeyProperty: infomaniak_api_key
        models:
          - name: gemma-4-31b
            model: google/gemma-4-31B-it
            maxTokens: 8192
```

Each `models` entry becomes one model of the gateway named
`<provider>/<model.name>` — `infomaniak/gemma-4-31b` above. The prefix is
deliberate: upstream ids collide across providers, and a tenant reading a model
list should be able to see who serves what. `mode` says what the model does
(`chat`, the default, `completion` or `embedding`); `maxTokens` is the context
window the gateway advertises.

**The claim is the only way in.** The gateway's model list is its
configuration file, and the file is written from the claim: a commit that adds
an entry adds the model, one that removes it removes the model, and each
replaces the gateway's pods, which read the file when they start. The gateway
runs with `STORE_MODEL_IN_DB` off, so it loads no model from its database and
refuses to store one — nothing typed into its console or sent to its API
becomes a model. No step of the installer, no Job and no controller is
involved; Argo CD syncs the claim and the chart together.

### Changing the models

The cluster's administrator changes them on the **Models** tab of the
administration console, or by command:

```bash
kubectl gentian models list                  # each model, and what the claim says of it
kubectl gentian models show > models.json    # the settings, as `set` takes them
kubectl gentian models set -f models.json    # make the claim declare exactly these
```

Both ask the director, and the director is the only writer:

| Route | Who may | What it does |
|---|---|---|
| `GET /v1/clusters/{c}/models` | `can_audit` on the cluster | The settings (`enabled`, `gpuAcceleration`, `console`, `instances`, `providers`) and each model under the gateway's name, with what the claim says of it |
| `PUT /v1/clusters/{c}/models` | `can_configure` on the cluster | Replaces the settings in the claim, as one signed commit in the person's name |

A tenant's administrator holds neither relation, and the routes exist under
`/v1/clusters` only. The console and the command write nothing to git and do
not reach the gateway.

The `PUT` carries the whole of the settings. A model, an instance or a provider
that the body does not name is removed from the claim, and with it from the
gateway, whoever wrote it there. The director holds the body to the schema of
`spec.llm` it was built with — names, `https` addresses, the modes — and also
refuses an empty model id, text over more than one line, and two entries that
would give the gateway one model name twice. A body with any other field is
refused unread: a provider's token is not set here. The
gateway's console switch (`console.enabled`, "Operator access" below) is part
of the settings; a body that leaves it out leaves it as the claim has it.
The rest of the claim, its
comments included, stays as it was; a comment written inside the list of
instances or of providers is lost when that list changes.

**A provider's address is a public one.** The gateway sends a provider's
token and every prompt to the provider's address, so the director holds it to
the rules it holds a catalog's address to
(`internal/director/catalogue/address.go`): `https`, no port but 443, no user
name or password, no query, and a host that is not a name or an address inside
the cluster or a private network. When an address is new to the claim the
director also resolves its host and refuses it unless every address it
resolves to is public. An address the claim already carries is not resolved
again. The gateway, not the director, connects to the provider and resolves
the name each time; nothing checks that connection, and `system-llm` has no
egress policy.

**A provider reads its own token.** All providers' tokens are properties of
one credential path, and a provider's is `<name>_api_key`, with `-` in the
name as `_` (`infomaniak_api_key`). `apiKeyProperty` on the claim has to say
exactly that. It is held in two places:

- the director refuses a provider that names any other property;
- the gateway's chart offers no model of such a provider
  (`gateway-config.yaml`), so a claim edited by hand cannot make the gateway
  send one provider's token to another provider's address either. The
  director's `GET`, the console and `kubectl gentian models list` show its
  models as not offered, with the reason.

What stays the administrator's to do: whoever may configure the cluster may
change the address of a provider, and the gateway then sends that provider's
token to the new, public, address.

**A provider's credential comes with the claim.** The Cluster composition
declares, for every provider under `spec.llm.providers` of a cluster that
serves models, the credential requirement `llm-provider-<name>` and its probe
(`crossplane/compositions/cluster-default.yaml`). Naming a provider is enough
for its token to have a place on the console's Credentials tab: no edit of
`credentials.yaml`, no installer step. It appears once the cluster has applied
the claim; a provider removed from the claim loses the requirement, and a
token stored for it stays in OpenBao.

### Whether a model works

The console shows it per model, and asks neither the gateway nor any pod of
`system-llm`:

| Model | Shown as | Known from |
|---|---|---|
| An instance's, with `gpuAcceleration` true | **Served** | The director, from the claim: the platform runs its instance (§6). Not "works": nothing asks whether the model has finished loading |
| An instance's, with `gpuAcceleration` false; any model with `enabled` false | **Not offered** | The director, from the claim: the gateway does not list it |
| A provider's, where the provider names a token property that is not its own | **Not offered** | The director, from the claim: the gateway's chart leaves such a provider out |
| A provider's, with no credential `llm-provider-<name>` on the cluster yet, or one that lacks the provider's property | **No credential** / **Property missing** | The custodian's list of credentials, which the console reads already |
| A provider's, whose credential is not satisfied | **Token missing** | The same list |
| A provider's, whose token is there | **Token supplied** | The same list. Not "works": nothing probes the token or the address |

`kubectl gentian models list` shows the director's part only.

### Seeing what the gateway was given

To see what the gateway was given, and what an app is offered:

```bash
# The file the gateway reads.
kubectl -n system-llm get configmap litellm-config -o jsonpath='{.data.config\.yaml}'

# The list an app sees, with the app's own key, from the app's pod.
kubectl -n <tenant namespace> exec deploy/<app> -- sh -c \
  'curl -s -H "Authorization: Bearer $OPENAI_API_KEY" "$OPENAI_API_BASE/models"'
```

The second needs an app that declared the gateway (`requires.services.llm`)
and whose image has `curl`; the variables are the ones the Secret
`llm-credentials-<app>` carries, under the names the app's profile maps them
to.

### The API key is a credential, the product id is not

Each provider has its own `llm-provider-<name>` credential declaring one field,
`<name>_api_key`, and they all share the OpenBao path
`gentian-os/kernel/llm-providers`. Supply the token in the **administration
console** under that credential — the write happens as your own OpenBao token,
merge-patches the path so it cannot clobber another provider's key, and tells
the ExternalSecret to resync immediately rather than at the end of its refresh
interval.

One requirement per provider rather than one with a field each, because
`checkFields` requires every declared field in a single write: a combined
credential would make the console demand every provider's token at once and
refuse a single rotation.

The requirements are not listed in `credentials.yaml`. The Cluster composition
declares them from the claim ("Changing the models", above), and what a claim
can make of that is fixed in the composition: the name is
`llm-provider-<name>`, the path is the one above, and the field is computed
from the provider's name — the claim's `apiKeyProperty` is not read there. A
claim cannot declare a requirement of any other name, on any other path, or
with another provider's field. A requirement gives nobody a right to write:
the custodian writes as the person, under that person's own OpenBao policy.

**The token never leaves its Secret.** The ExternalSecret
`llm-provider-credentials` (rendered when the claim names a provider) copies
the vault path into a Secret of the gateway's namespace, the gateway's
container mounts it, and sets one environment variable per property when it
starts, `LLM_PROVIDER_KEY_<apiKeyProperty>`. The configuration file names the
variable, not the token, and nothing writes the token to the gateway's
database.

**A model whose token is missing is listed and does not answer.** Until the
token is supplied a call to such a model comes back as an authentication
error from the gateway. The administration console flags it on the Models tab
(above); no status or alert on the cluster reports the gap. When the token is
supplied, rotated or removed, the gateway's container restarts by itself to
read it: a probe compares the mounted Secret with what the container started
with. That takes up to a few minutes — the ExternalSecret's resync, the
kubelet bringing the Secret into the pod, the probe's period — and both
replicas may restart together, so the gateway can be away for the time it
takes to start.

> These credentials declare `validate: noop`, so the console stores the token
> without probing it — the validator enum has no generic bearer-token check.
> Nothing else probes it either: a wrong token, a token without the provider's
> scope and an `apiBase` that is not reachable from the cluster all show as a
> failing call, not as a report.

The product id in `apiBase` is not secret: it selects which product is billed and
it is part of the endpoint, so it belongs on the claim where it can be reviewed.
For Infomaniak it is the AI Tools product id (`GET /1/ai` returns it), not an
account or user id, and the token needs the `ai-tools` scope — one without it
authenticates and then refuses every AI endpoint.

### Where a provider can be reached from

Only from the gateway. The policies on `system-llm` restrict who may connect to
its pods and say nothing about egress, so LiteLLM reaches the internet; a tenant
namespace is denied egress by default (`tenant-isolation`), so a tenant app
configured to call a provider directly gets a timeout its UI usually renders as
an empty model list. That asymmetry is the design: the gateway is what holds the
credential.

> Adding an egress NetworkPolicy to `system-llm` to "allow 443" would be a
> regression, not a hardening: the namespace has no egress policy, and the
> first one switches the selected pods to deny-by-default, breaking LiteLLM's
> DNS, Postgres and Redis unless all of it is enumerated in the same policy.

### Operator access

LiteLLM's Admin Console is **off unless the Cluster claim switches it on**:

```yaml
spec:
  llm:
    enabled: true
    console:
      enabled: true     # default false
```

Off, the default, there is no route and no published host for
`llm.<kernelDomain>`, and the gateway's NetworkPolicy has no rule for the edge:
the gateway is reached from inside the cluster only, by the apps and desktops
that declared it and by the operator. Nothing the platform does needs the
console. Models come from the claim, an app's key and a tenant's team are
registered by the operator, and a provider's token is entered in the
administration console.

On, `llm.<kernelDomain>` is routed to the gateway behind the kernel sign-in for
accounts that may configure the cluster (`can_configure`;
`kernel_gateway_routes.go`), the host is published, the edge's Envoy pods are
admitted to the gateway's port, and such an administrator's desktop shows a
**Model gateway** tile, projected from the route like the other kernel
consoles' (`tile_projection_reconciler.go`). Platform administrators only,
because the routing and budgets there apply to every tenant. The models are
still the claim's and the console cannot add one; it is for inspecting them,
keys and spend. The host label `llm` stays reserved either way.

The switch is in the claim, and on the Models tab of the administration
console, with a warning beside it. Both halves follow the commit to the claim,
with no run of the install: the operator makes or removes the route once the
claim is applied, and the gateway's chart, which is handed the claim as a
values file, renders or drops the rule that admits the edge to the gateway
once Argo CD syncs its Application. The two are separate syncs of one commit;
between them a route without the rule answers nothing, and a rule without the
route admits pods that have nothing to route.

Two things to know before switching it on:

- **It is a departure from "system services have no public route"**, taken by
  the cluster's owner for this cluster. The route forwards `/`, so the
  gateway's whole API is on that host as well as its pages — for a signed-in
  platform administrator only, and LiteLLM's own key check still applies behind
  the session.
- **The console signs in to LiteLLM by itself.** The kernel session opens the
  host; LiteLLM then asks for its own administrator sign-in (the master key),
  and its pages send the key that gives them in the `Authorization` header.
  The edge leaves that header alone on this route, as it does on Keycloak's
  administration console, and passes no token of the platform's to LiteLLM
  ([routing.md §4.1](routing.md)).

---

## 6. Models the cluster serves itself

`spec.llm.instances` names the models a cluster with GPUs serves from its own
weights, one vLLM instance each. It is read only when `gpuAcceleration` is
true.

```yaml
spec:
  llm:
    enabled: true
    gpuAcceleration: true
    instances:
      - name: qwen
        modelId: Qwen/Qwen2.5-7B-Instruct
```

The gateway offers each entry as a model named after `modelId`, in lower case
with `/` as `-` — `qwen-qwen2.5-7b-instruct` above — and calls it at
`http://vllm-<name>-inference.system-llm.svc.cluster.local:8000/v1`. That is
the same file and the same mechanism as for a provider's models (§5).

**The instance is the gateway chart's** (`kernel/services/llm/manifests/templates/vllm.yaml`),
read from the same claim: per entry a Deployment on a node with
`nvidia.com/gpu.present`, one `nvidia.com/gpu`, its Service, and a PVC for the
weights that is kept when the entry is removed. The pod runs as a user that is
not root with the kernel's admission baseline, its caches under the PVC
(`HOME`, `HF_HOME`), and the NetworkPolicy `llm-vllm-ingress` admits the
gateway alone. The optional Secret `vllm-hf-token`, which holds the Hugging
Face token for gated models, is created by hand.

With `gpuAcceleration` true the mock model server is not started. With it
false the mock runs, and the gateway does not offer it as a model: a cluster
without GPUs and without providers has a gateway with an empty model list.

