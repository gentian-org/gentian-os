# UPIR v0.1 — Unified Procedural Intermediate Representation

**Status:** Draft. First release.
**Date:** August 2026

---

## 1. Purpose and scope

UPIR is the runtime instruction set for a unified process platform spanning process orchestration, integration automation, agent orchestration and durable execution. It is not an authoring notation and is not intended to be hand-written. Authoring surfaces — BPMN diagrams, DAG editors, YAML DSLs, and LLM agents — compile *into* UPIR; a single orchestrator interprets it.

Design targets, in priority order:

1. **Executable completeness** for business processes, including human decisions and compensation.
2. **Statically checkable** — malformed programs unrepresentable or rejected before execution.
3. **Machine-generable** — an LLM must emit valid UPIR reliably under schema-constrained decoding.
4. **Small** — every instruction must correspond to a distinct engine behaviour.

Non-goals: superset of BPMN, arbitrary computation, human-readable authoring.

### 1.1 Terminology

RFC 2119 keywords apply.

- **Definition** — a versioned UPIR document.
- **Instance** — one execution of a definition.
- **Scope** — a lexically nested block owning variables, error handlers and a compensation stack.
- **Step** — a durably-recorded unit of engine work.
- **Job** — a unit of work handed to an external worker.
- **Worker** — a stateless process that claims jobs and reports results.
- **Path** — the scope-qualified address of an instruction, e.g. `/checks/branch_2/approval`.

---

## 2. Structural model

UPIR definitions are **block-structured**. Instructions form nested scopes; arbitrary edges are not representable.

Consequences accepted: unstructured cycles, cross-boundary jumps and free-graph topology cannot be expressed. Compilers from such notations MUST reject unstructurable definitions with a specific diagnostic.

Benefits: unreachable instructions and dangling references are unrepresentable; variable, error and compensation scope are one nesting; generation by language models is materially more reliable; and instance position is a **path**, which makes version diffing a tree comparison (§10).

### 2.1 Document envelope

```yaml
upir: "0.1"
id: "invoice.approval"
version: 4
name: "Invoice Approval"
inputs:
  invoice:
    type: record
    fields:
      id:     { type: string }
      amount: { type: decimal }
      vendor: { type: string }
outputs:
  decision: { type: enum, values: [approved, rejected] }
capabilities:
  allow: [invoke, set, switch, gate, wait, end]
  invoke_targets: ["erp.*", "mail.send"]
body:
  - id: fetch
    op: invoke
    # ...
```

`id` + `version` is the primary key. Running instances are pinned to their starting version unless explicitly migrated (§10).

### 2.2 Common instruction fields

| Field | Type | Meaning |
|---|---|---|
| `id` | string | REQUIRED. Unique within enclosing scope. Stable across versions where possible. |
| `op` | enum | REQUIRED. One of eleven. |
| `previous_ids` | list\<string\> | Former IDs, used to auto-generate migration mappings (§10.3). |
| `in` | map\<string, expr\> | Input mapping, evaluated in the enclosing scope. |
| `out` | map\<string, expr\> | Output mapping written back to the enclosing scope. |
| `guard` | expr → bool | If false the instruction is skipped and recorded as `skipped`. |
| `retry` | RetryPolicy | §5.2. |
| `catch` | list\<CatchClause\> | §5.3. |
| `timeout` | duration | Wall-clock limit; raises `upir.timeout`. |
| `compensate_with` | Instruction | Registered on success, unwound in reverse on scope failure. §5.4. |
| `idempotency_key` | expr → string | Deduplication key for at-most-once external effects. |
| `concurrency_key` | expr → string | Named mutex / rate-limit scope, implicitly tenant-namespaced. |
| `metadata` | map\<string, string\> | Labels, audit annotations, source-notation provenance. |

---

## 3. Type system

All variables, inputs, outputs and expressions are typed.

### 3.1 Scalars

| Type | Notes |
|---|---|
| `null` | Unit. |
| `bool` | |
| `int` | 64-bit signed. |
| `decimal` | Arbitrary precision, base 10. Monetary values MUST use this. |
| `string` | UTF-8. |
| `datetime` | RFC 3339, timezone-aware. |
| `duration` | ISO 8601. |
| `bytes` | Base64 in transport; SHOULD be avoided in favour of `ref`. |

**There is no float scalar.** IEEE 754 cannot exactly represent most decimal fractions, accumulates error across summation, is non-associative (so parallel `fork` branches can produce different totals), and breaks equality comparison. Any float-typed value in an IR will eventually be used for money by someone under deadline; making it unrepresentable means the compiler prevents that. Floating-point data is still fully supported — it lives *inside* `opaque` payloads (§3.5), where the orchestrator never performs arithmetic on it.

### 3.2 Composites

`record` · `array<T>` · `map<string,T>` · `enum` · `optional<T>` · `union<T…>`

Absence of `optional` means required.

### 3.3 Collections

`list<T>` is a **mutable, append-only accumulator**. It exists to aggregate iteration results (§4.5) and is the only mutable structure in UPIR.

```yaml
type: list
of: { type: record, fields: { index: { type: int }, error: { type: error } } }
max_length: 10000        # REQUIRED — unbounded growth is not representable
spill_to_ref_above: 1000 # optional; larger lists move out of instance state
```

Rules:

- Permitted operation: **append**. No insert, delete, or in-place update.
- A `list<T>` is **sealed into an `array<T>`** when its owning scope exits. Downstream instructions see an immutable array.
- Appends carry the originating iteration index, so ordering is **by input index, not completion order**. Parallel iteration is therefore deterministic on replay.
- Exceeding `max_length` raises `upir.collection_overflow`.
- When `spill_to_ref_above` is exceeded the list is written to a `ref` and instance state holds the handle. Instance state must stay small enough that every checkpoint is cheap.

### 3.4 Domain types

**`error`**
```yaml
type: error
fields:
  code: string          # dotted namespace, e.g. "erp.invoice.locked"
  message: string
  retryable: bool
  details: optional<map<string, string>>
```

**`ref`** — an opaque handle to content held outside instance state.
```yaml
type: ref
fields:
  kind: enum [document, blob, secret, dataset, collection]
  uri: string
  digest: string           # content address, e.g. "sha256:9f2a…"
  content_type: optional<string>
  size_bytes: optional<int>
```
Refs MUST be immutable and content-addressed. Mutable refs break replay determinism. Secrets MUST be `ref` and MUST NOT be resolvable by expressions — only by workers holding the corresponding capability.

**`principal`**
```yaml
type: principal
fields:
  kind: enum [user, group, role, service, agent]
  id: string
  tenant: string
  display_name: optional<string>
```

### 3.5 `opaque` — payloads the orchestrator does not interpret

```yaml
type: opaque
encoding:
  media_type: "application/vnd.safetensors"
  schema: "torch.tensor/v1"      # stable versioned identifier
  dtype: "float32"               # descriptive only
  shape: [1, 3, 224, 224]        # descriptive only
storage: ref                     # inline | ref
digest: "sha256:9f2a…"
```

The governing rule: **control flow is typed; payloads are opaque.** Floats, tensors, model checkpoints, Arrow tables and configuration objects may exist inside opaque values; they may never be values the orchestrator reasons about.

| The orchestrator MAY | The orchestrator MUST NOT |
|---|---|
| Copy and pass between instructions | Index into the payload |
| Compare digests for equality and caching | Perform arithmetic on it |
| Check encoding compatibility at boundaries | Evaluate it in a `guard` or `switch` case |
| Iterate over `array<opaque>` | Coerce it to a scalar |
| Garbage-collect by reference count | Dereference it in any expression |

**The expression language MUST NOT dereference into an opaque payload.** This single rule preserves the no-float guarantee: a float inside a tensor can never reach a control-flow decision.

`storage: inline` is permitted for small self-describing values (a config object, a hyperparameter set). A size threshold auto-promotes inline to `ref`.

**Encoding allowlist.** The engine maintains a registry of permitted encodings. `pickle` and any other encoding whose deserialisation permits arbitrary code execution MUST NOT be registered. Recommended: safetensors, Arrow, Parquet, ONNX, npy, JSON, Protobuf, msgpack. Enforcement is in the registry, not in worker code.

**Projection pattern.** Workers return both the artifact and the scalars needed for control flow. The projection is a deliberate authoring act:

```yaml
- id: train
  op: invoke
  target: "ml.train"
  in:
    dataset: "dataset"          # opaque, ref
    config:  "hyperparams"      # opaque, inline
  out:
    model:    "result.model"      # opaque — stays opaque
    val_loss: "result.val_loss"   # decimal — projected for control flow
    epochs:   "result.epochs"     # int

- id: quality_gate
  op: switch
  cases:
    - guard: "val_loss < 0.15"
      body:
        - id: register
          op: invoke
          target: "ml.registry.push"
          in: { model: "model" }
  default:
    body:
      - id: reject
        op: end
        status: fail
        error: { code: "ml.quality_gate", message: "loss above threshold", retryable: false }
```

### 3.6 Error taxonomy

Reserved `upir.*` codes:

| Code | Raised when |
|---|---|
| `upir.timeout` | Instruction `timeout` expired. |
| `upir.cancelled` | Instance or scope cancelled. |
| `upir.validation` | Type or schema validation failed. |
| `upir.expression` | CEL evaluation error or cost limit exceeded. |
| `upir.unavailable` | No worker available within the job deadline. |
| `upir.capability_denied` | Instruction or target not permitted. |
| `upir.plan_rejected` | A `plan` fragment failed validation or policy. |
| `upir.collection_overflow` | `list<T>` exceeded `max_length`. |
| `upir.encoding_mismatch` | Opaque encoding incompatible with target's declared interface. |
| `upir.compensation_failed` | A compensating instruction failed during unwind. |
| `upir.migration_blocked` | Instance could not be migrated. |

`catch` matches on prefix.

---

## 4. Instruction reference

### 4.1 `invoke`

Calls something outside the orchestrator: a worker, connector, sandboxed script, MCP tool, or remote A2A agent.

```yaml
- id: post_to_erp
  op: invoke
  target: "erp.invoice.create"
  target_kind: worker         # worker | tool | agent | sandbox
  protocol: native            # native | mcp | a2a
  in:
    amount: "invoice.amount"
    vendor: "invoice.vendor"
  out:
    erp_id: "result.id"
  idempotency_key: "invoice.id"
  retry: { max: 5, backoff: exponential, initial: PT2S }
  compensate_with:
    id: void_erp
    op: invoke
    target: "erp.invoice.void"
    in: { id: "erp_id" }
```

| Field | Notes |
|---|---|
| `target` | Logical address. Resolution is a runtime concern. |
| `target_kind` | `worker` (default) · `tool` (MCP) · `agent` (A2A) · `sandbox` (untrusted code). |
| `protocol` | `native` · `mcp` · `a2a`. See §9. |
| `mode` | `sync` (default, awaits) · `async` (completes on external callback). |
| `deadline` | Maximum time a job may remain unclaimed. |
| `transactional` | Boolean. When true and the target operates on the same PostgreSQL instance, the step checkpoint and the target's writes commit in one transaction. See §8.3. |
| `resources` | Scheduler hints: `{ gpu: "a100", gpu_count: 2, memory: "64Gi" }`. Metadata to the orchestrator. |
| `cache` | `{ enabled: true, ttl: P7D }` — skip execution when input digests and target version are unchanged. Requires all inputs to be content-addressed. |

**The orchestrator MUST NOT perform I/O.** `invoke` emits a job and awaits it. This is the central operational invariant: the orchestrator can never be blocked by a slow external system.

**Interface declarations** enable structural type checking without the orchestrator understanding payload contents:

```yaml
target: "ml.train"
accepts:
  dataset: { encoding: "arrow.table/v1" }
  config:  { encoding: "gentian.trainconfig/v2" }
produces:
  model:   { encoding: "torch.tensor/v1" }
```

Wiring a `torch.tensor/v1` into a parameter expecting `onnx.model/v1` fails at compile time with `upir.encoding_mismatch`.

### 4.2 `set`

Pure state transformation. No I/O, no job, no worker, no external failure mode.

```yaml
- id: compute_totals
  op: set
  net:   "invoice.amount"
  tax:   "invoice.amount * decimal('0.081')"
  gross: "invoice.amount * decimal('1.081')"
```

Field names other than the reserved common fields (§2.2) are variable assignments. Kept distinct from `invoke` precisely because it cannot fail externally and needs no durable job. `set` instructions are coalesced into the next checkpoint rather than each producing a durable write (§8.4).

### 4.3 `switch`

N-way exclusive choice.

```yaml
- id: route_by_amount
  op: switch
  cases:
    - guard: "invoice.amount > decimal('10000')"
      body: [ ... ]
    - guard: "invoice.amount > decimal('1000')"
      body: [ ... ]
  default:
    body: [ ... ]
```

Guards evaluate in order; the first true case is taken; exactly one path executes. If none match and `default` is absent, `upir.validation` is raised.

### 4.4 `fork`

Parallel branches with an explicit join policy.

```yaml
- id: parallel_checks
  op: fork
  join: all                        # all | any | n
  n: 2                             # required when join = n
  on_branch_error: cancel_siblings # cancel_siblings | continue | fail_fast
  branches:
    - id: credit_check
      body: [ ... ]
    - id: compliance_check
      body: [ ... ]
```

The join is a **field of the fork**, not a separate instruction, making mismatched split/join pairs unrepresentable. `any` and `n` cancel remaining branches by default.

### 4.5 `iterate`

Map over a collection, or loop while a condition holds.

```yaml
- id: process_lines
  op: iterate
  mode: parallel                # sequential | parallel
  over: "invoice.lines"
  as: line
  index_as: i
  max_concurrency: 5
  on_item_error: collect        # fail_fast | collect | skip
  collect:
    results: "item_result"      # appended per successful item
    errors:  "item_error"       # appended per failed item
    max_length: 10000
  body: [ ... ]
  out:
    ok:      "collected.results"
    failed:  "collected.errors"
    summary: "collected.summary"
```

**Aggregation shape.** Where `on_item_error: collect`, the engine maintains three values in the iteration scope:

| Value | Type | Contents |
|---|---|---|
| `collected.results` | `list<optional<T>>` → sealed to `array<optional<T>>` | Index-aligned with the input collection. `null` at indices that failed or were skipped. |
| `collected.errors` | `list<record{index: int, item: T, error: error}>` → sealed to `array<…>` | One entry per failed item, ordered by input index. |
| `collected.summary` | `record{total: int, succeeded: int, failed: int, skipped: int}` | Computed on seal. |

Ordering is by input index, never completion order, so parallel iteration is deterministic on replay.

While-loop form:

```yaml
- id: poll_until_ready
  op: iterate
  mode: sequential
  while: "status != 'ready'"
  max_iterations: 100           # REQUIRED — unbounded loops are not representable
  body: [ ... ]
```

### 4.6 `wait`

Suspend until a time, or until a matching event is delivered by the pub-sub layer (§6). Costs nothing while suspended.

```yaml
- id: wait_for_payment
  op: wait
  subscribe:
    topic: "payment.received"
    correlate_on: "invoice.id"
    from: buffered              # buffered | live
    since: "instance.started_at"
    consume: true
    payload_as: payment
  timeout: P30D
  catch:
    - on: "upir.timeout"
      body: [ ... ]
```

| Form | Field |
|---|---|
| Duration | `duration: PT1H` |
| Absolute | `until: "invoice.due_date"` |
| Event | `subscribe: { … }` — see §6 |
| Cron | `cron: "0 9 * * MON"` — valid only as the first instruction of a definition |

Multi-event form:

```yaml
- id: gather_approvals
  op: wait
  subscribe:
    topic: "department.signoff"
    correlate_on: "invoice.id"
    count: 3                    # complete when 3 matching events received
    window: P5D
    collect_as: signoffs        # array<payload>, arrival-ordered
  timeout: P7D
```

### 4.7 `gate`

A human decision. Distinct from `wait` because assignment, claiming, delegation, escalation, quorum and audit-of-who-decided are core business process requirements, and folding them into a generic event wait makes each of them ad hoc.

```yaml
- id: manager_approval
  op: gate
  assign_to:
    - { kind: role, id: "finance.manager" }
  quorum: any                   # any | all | <int>
  form: "forms/invoice-approval@2"
  decisions: [approve, reject, request_changes]
  allow_edit: ["invoice.cost_centre"]
  due: P3D
  escalate:
    after: P3D
    to: { kind: role, id: "finance.director" }
  out:
    decision:   "decision"
    decided_by: "principal"
    comment:    "comment"
```

The engine MUST record `decided_by` (a `principal`), decision timestamp, and any edits in instance history.

### 4.8 `call`

Invoke another definition. Creates an identified child instance.

```yaml
- id: run_kyc
  op: call
  definition: "kyc.check"
  version: 4                    # pinning strongly recommended over "latest"
  mode: await                   # await | detach
  in:  { subject: "invoice.vendor" }
  out: { kyc_result: "result" }
```

### 4.9 `emit`

Publish an event. Fire-and-forget, **no addressee**.

```yaml
- id: announce
  op: emit
  topic: "invoice.approved"
  correlate_on: "invoice.id"
  payload:
    id:     "invoice.id"
    amount: "invoice.amount"
  scope: tenant                 # instance | tenant | external
  transport: native             # native | a2a
```

`call` and `emit` differ in who executes and whether an addressee exists:

| | Executes where | Addressee | Returns | Recipients |
|---|---|---|---|---|
| `invoke` | External worker / tool / remote agent | named target | yes | exactly 1 |
| `call` | This engine, new child instance | named definition | yes | exactly 1 |
| `emit` | Nowhere — publishes to a topic | **none** | no | 0..n |

### 4.10 `end`

Terminate the enclosing scope.

```yaml
- id: done
  op: end
  status: success               # success | fail | terminate
  out: { decision: "approved" }
```

`fail` raises into the enclosing `catch` chain and triggers that scope's compensation. `terminate` ends the entire instance immediately without compensation and SHOULD be rare.

### 4.11 `plan`

An agent emits a UPIR fragment, which is validated, capability-checked, optionally human-approved, then spliced into the running instance.

```yaml
- id: resolve_exception
  op: plan
  agent: "agents.invoice-exception"
  in:
    context: "invoice"
    history: "exception_log"
  capabilities:
    allow: [invoke, set, switch, gate, end]
    invoke_targets: ["erp.invoice.*", "mail.send"]
    max_instructions: 20
    max_depth: 3
    forbid_compensation_removal: true
  approval:
    required_when: "estimated_cost > decimal('500')"
    assign_to: [{ kind: role, id: "finance.manager" }]
    render_as: bpmn
  max_rounds: 5
  out:
    outcome: "result"
```

**Fragment identity.** Every emitted fragment is content-addressed:

```
fragment_digest = sha256( JCS(fragment) )                    # RFC 8785 canonicalisation
fragment_id     = "{plan_instruction_path}#{round}:{fragment_digest[0:12]}"
```

Example: `/resolve_exception#3:9f2a1c4d8b0e`

Instruction paths inside a fragment are namespaced by the fragment ID:
`/resolve_exception#3:9f2a1c4d8b0e/notify_vendor`

This yields four properties: stable audit identity across re-planning rounds; replay determinism (the same fragment always addresses identically); caching (an identical fragment digest for the same context may reuse a prior validation result); and migration can positively identify plan-generated scopes (§10.6).

Both `fragment_digest` and `fragment_id` are recorded in instance history alongside the model, prompt digest and token usage.

Execution semantics:

1. The agent is invoked as a durable step with the given context.
2. It returns a UPIR fragment — instructions only, no envelope.
3. The fragment is canonicalised and digested, then **schema-validated**, then **capability-checked**, then **policy-gated**.
4. If `approval.required_when` is true, a `gate` is synthesised; the human sees the fragment, optionally rendered as BPMN.
5. The fragment executes in a child scope of the `plan` instruction.
6. Results return to the agent. If `max_rounds` is not exhausted and the fragment did not `end`, the loop repeats.

Rules:

- The fragment MUST NOT contain `plan` unless `capabilities.allow` includes it. Recursive planning is off by default.
- Fragment capabilities MUST be a subset of the enclosing scope's. Privilege can only narrow.
- The fragment MUST NOT remove or override `compensate_with` registered by ancestor scopes when `forbid_compensation_removal` is set.
- Validation failure raises `upir.plan_rejected`, which is catchable and SHOULD be fed back to the agent as a repair prompt rather than failing the instance.
- **There is no generic code-execution instruction.** An agent needing to run code emits `invoke` with `target_kind: sandbox`, subject to the same capability check as any other target.

**Iterative planning is the intended mode.** Forcing a complete branch-exhaustive plan up front is compiling blind, and language models are poor at exhaustive branch enumeration. Emit a fragment, execute, observe, re-plan.

---

## 5. Execution semantics

### 5.1 State model

The orchestrator is a pure function:

```
(instance_state, event) → (next_instructions, new_state)
```

It performs no I/O. Every instruction transition is a checkpoint. Recovery after a crash is replay from the last checkpoint. Persistence is specified in §8.

### 5.2 Retry

```yaml
retry:
  max: 5
  backoff: exponential          # fixed | linear | exponential
  initial: PT1S
  max_interval: PT5M
  jitter: true
  on: ["upir.unavailable", "erp.*"]   # default: any error with retryable = true
```

Retries do not create new instruction IDs; attempt count is part of step history. `idempotency_key` SHOULD be set on any `invoke` with external side effects.

### 5.3 Catch

```yaml
catch:
  - on: "erp.invoice.locked"
    body: [ ... ]
  - on: "upir.timeout"
    body: [ ... ]
  - on: "*"
    as: err
    body: [ ... ]
```

Prefix matching, most specific first. Unhandled errors propagate to the enclosing scope, triggering that scope's compensation.

### 5.4 Compensation

Business processes must **undo**, not merely re-run. A data pipeline that fails halfway is re-run from the start; a process that has reserved stock, charged a card and failed to book a courier cannot be. Compensation composes forward operations with explicit undo operations, unwound in reverse (Garcia-Molina and Salem's saga pattern, 1987).

- On successful completion of an instruction carrying `compensate_with`, the compensating instruction is pushed onto the enclosing scope's compensation stack, **capturing the values it needs at that moment**.
- If the scope subsequently fails, the stack unwinds in reverse order before the error propagates.
- Compensating instructions run with captured values, not current state.
- A failing compensation raises `upir.compensation_failed` and MUST be surfaced; it does not silently continue.
- The compensation stack is durable. A crash mid-unwind resumes the unwind, not the forward path.

Artifact-producing steps SHOULD compensate by deleting the artifact or unregistering the version, or failed runs leave orphaned large objects.

### 5.5 Cancellation

Cancelling an instance or scope cancels in-flight jobs, releases timers and subscriptions, then runs compensation for completed instructions unless `compensate: false` is set on the cancellation request.

---

## 6. Events: durable pub-sub with buffering

The naive design — a subscription created when `wait` is reached — loses events that arrive first, and cannot serve multiple waiters. UPIR specifies a durable topic layer with retention.

### 6.1 Topic declaration

Topics are declared at tenant or definition level:

```yaml
topics:
  - name: "payment.received"
    key: "invoice_id"           # path in the payload forming the correlation key
    payload:
      type: record
      fields:
        invoice_id: { type: string }
        amount:     { type: decimal }
    delivery: fanout            # fanout | exclusive
    buffer:
      retain: PT72H             # retention window for late subscribers
      max_per_key: 100
      on_overflow: drop_oldest  # drop_oldest | reject
    scope: tenant               # tenant | external
```

### 6.2 Delivery semantics

| `delivery` | Behaviour |
|---|---|
| `fanout` | Every matching subscription receives the event. Default for notifications. |
| `exclusive` | Exactly one subscription consumes it, ordered by subscription creation time. Remaining waiters continue waiting. Used for work distribution. |

### 6.3 Subscription

A `wait` with a `subscribe` block registers a **durable subscription** at instruction entry:

```yaml
subscribe:
  topic: "payment.received"
  correlate_on: "invoice.id"    # CEL expression producing the key value
  from: buffered                # buffered | live
  since: "instance.started_at"  # replay window lower bound
  consume: true                 # for exclusive topics, marks the event consumed
  payload_as: payment
  count: 1                      # optional, >1 waits for N events
  window: P5D                   # optional, time window for multi-event collection
  collect_as: signoffs          # required when count > 1
```

### 6.4 Race resolution

The ordering problem is resolved by retention plus replay window:

1. `emit` writes the event to the topic buffer under its correlation key, with a durable event ID and timestamp, then delivers to all currently-matching subscriptions.
2. When a subscription is created with `from: buffered`, the engine **first scans the buffer** for events matching the key with timestamp ≥ `since`, before waiting for live delivery.
3. Delivery is deduplicated on `(subscription_id, event_id)`, so an event present in both the buffer scan and live delivery is delivered once.
4. `from: live` skips the buffer scan — appropriate when only events after subscription creation are meaningful.

This makes the common failure mode — a downstream system responding faster than the process reaches its `wait` — a non-event.

### 6.5 Retention and cleanup

Buffered events are deleted when `retain` expires or when `max_per_key` is exceeded per `on_overflow`. Retention is a tenant-level cost control; long-lived processes SHOULD set `retain` at least as long as their expected `wait` duration.

---

## 7. Expressions: CEL

UPIR uses **Common Expression Language** (CEL).

Rationale: non-Turing-complete, side-effect free, statically type-checkable against a declared environment, with built-in cost accounting for bounded evaluation. Implementations exist for Go, Java, C++, Rust and Python.

### 7.1 Type mapping

| UPIR | CEL |
|---|---|
| `bool`, `int`, `string`, `bytes` | native |
| `datetime` | `google.protobuf.Timestamp` |
| `duration` | `google.protobuf.Duration` |
| `record`, `map` | `map` / message type |
| `array<T>`, `list<T>` (sealed) | `list` |
| `enum` | `string` with a validated value set |
| `optional<T>` | `optional_type` (CEL optional extension) |
| `decimal` | **UPIR extension type** (§7.2) |
| `error`, `principal`, `ref` | messages with typed fields |
| `opaque` | **opaque handle — no accessors registered** |

### 7.2 The `decimal` extension

CEL has no native decimal. UPIR registers an extension type with:

- Construction: `decimal("0.081")` — from string only, never from a float literal.
- Arithmetic: `+ - * /` with decimal operands. Mixed `decimal`/`int` promotes the int. Mixed `decimal`/`string` is a type error.
- Comparison: `< <= > >= == !=`.
- `round(value, places, mode)` where mode ∈ `half_up`, `half_even`, `floor`, `ceiling`. Rounding is explicit; there is no implicit rounding.
- Division by zero raises `upir.expression`.

### 7.3 Restrictions

- **No accessors are registered for `opaque`.** Any expression attempting to index into an opaque payload fails at compile time.
- **No non-deterministic functions.** `now()`, `uuid()` and equivalents are not registered; replay must be deterministic. Time and identity are supplied as bound variables (§7.4) or obtained via `invoke`.
- **Cost limit.** Every expression is evaluated under a cost budget. Exceeding it raises `upir.expression`. Comprehensions are permitted but counted.
- **No I/O, no secret dereference.** `ref` fields are readable (`uri`, `digest`), contents are not.

### 7.4 Bound variables

| Name | Type | Available in |
|---|---|---|
| scope variables | as declared | all expressions |
| `instance.id`, `instance.started_at`, `instance.version` | string, datetime, int | all |
| `tenant.id` | string | all |
| `result` | instruction output | `out` mappings |
| iteration binding (`as`), index (`index_as`) | element type, int | `iterate` body |
| `item_result`, `item_error` | element type, error | `iterate` collect |
| `err` (via `catch … as`) | error | catch bodies |
| `collected.*` | as §4.5 | after `iterate` |

---

## 8. Persistence: the DBOS pattern

UPIR's reference persistence model is **PostgreSQL only** — no separate broker, no separate state store, no separate history store.

### 8.1 Rationale

Durable execution requires that workflow state survive crashes. Systems that hold state in a dedicated cluster (Temporal, Zeebe) achieve this at the cost of a second stateful system to operate and a two-phase problem between workflow state and application data. Holding workflow state in the same PostgreSQL as application data allows both to be committed in a single transaction, which eliminates the entire class of "the step succeeded but the checkpoint didn't" bugs.

### 8.2 Tables

| Table | Contents |
|---|---|
| `upir_instance` | Instance id, definition id + version, tenant, status, current paths, variables (jsonb), migration state |
| `upir_step` | Instance id, path, attempt, status, output (jsonb), idempotency key, lease owner, lease expiry, committed_at |
| `upir_queue` | Pending jobs: target, payload ref, priority, concurrency key, visible_after |
| `upir_timer` | Instance id, path, fire_at |
| `upir_subscription` | Instance id, path, topic, correlation key, since, consume flag, status |
| `upir_event` | Topic, correlation key, event id, payload (jsonb or ref), emitted_at, expires_at |
| `upir_delivery` | Subscription id, event id — dedup ledger |
| `upir_compensation` | Instance id, scope path, ordinal, instruction (jsonb), captured values (jsonb) |
| `upir_history` | Append-only audit: instance id, path, event type, principal, payload digest, at |

All tables are tenant-scoped. Schema-per-tenant is the recommended default; database-per-tenant for high-assurance tenants.

### 8.3 Transaction boundaries

- **Ordinary `invoke`.** The worker executes, reports the result, and the engine commits the step checkpoint. Exactly-once is approximated via `idempotency_key`, enforced by a unique index on `upir_step(instance_id, path, idempotency_key)`.
- **`transactional: true` `invoke`.** When the target operates on the same PostgreSQL instance, the target's writes and the step checkpoint commit in **one transaction**. This is genuinely exactly-once, with no idempotency key required. This is the DBOS pattern's central benefit and SHOULD be used for all internal database operations.
- **`set`.** Pure and replayable from prior state, so `set` instructions do not each produce a durable write. They are coalesced into the next checkpoint written by a non-pure instruction (§8.4).
- **`emit`.** The event row and the emitting step checkpoint commit together, so an emitted event can never exist without the step that emitted it, or vice versa.

### 8.4 Checkpoint coalescing

A run of consecutive `set` and `switch` instructions produces no durable writes. The engine records the resulting variable state as part of the next durable checkpoint. On recovery, the run is re-evaluated — deterministic and cheap, because neither instruction performs I/O.

This keeps checkpoint frequency proportional to external interactions rather than to instruction count.

### 8.5 Queueing and recovery

- Job dispatch uses `SELECT … FOR UPDATE SKIP LOCKED` against `upir_queue`.
- Workers hold a **lease** on a step. On expiry without completion the step is re-queued; `idempotency_key` or `transactional: true` prevents duplicate effects.
- On orchestrator start, instances with expired leases or fired timers are resumed. There is no separate recovery service.
- `concurrency_key` maps to an advisory lock or a counting row, tenant-namespaced.

---

## 9. Agent interaction: MCP and A2A

Two genuinely different interaction shapes require two bindings.

### 9.1 Tools — MCP

```yaml
- id: search_registry
  op: invoke
  target: "mcp://tools.internal/company-registry"
  target_kind: tool
  protocol: mcp
  in: { query: "invoice.vendor" }
  out: { registry_record: "result" }
```

MCP is the vertical contract: agent to tool. Tool invocations are ordinary durable steps, subject to `invoke_targets` capability checks, retries and compensation like any other call.

### 9.2 Agent collaboration — A2A over `invoke`

Where one agent depends on another's output — a request with a known addressee and an expected response.

```yaml
- id: request_legal_opinion
  op: invoke
  target: "a2a://legal-review.agency.example/agent"
  target_kind: agent
  protocol: a2a
  in:
    task: "review contract clause"
    context: "contract_ref"       # ref, not inline
  out:
    opinion: "result.artifact"
  timeout: PT4H
  retry: { max: 2, backoff: exponential, initial: PT30S }
```

Properties inherited from `invoke`: named addressee, so `invoke_targets` capability checking applies; typed response; durable step with retry, timeout and compensation; the remote agent's Agent Card is resolved at the target address.

### 9.3 Agent broadcast — A2A over `emit`

Where an agent publishes to an unknown set of recipients.

```yaml
- id: broadcast_finding
  op: emit
  topic: "compliance.finding"
  correlate_on: "case.id"
  payload:
    severity: "finding.severity"
    summary:  "finding.summary"
  scope: external
  transport: a2a
```

No addressee, no response, zero or more recipients. Remote agents subscribe through the same topic layer (§6), so buffering and correlation apply uniformly to local and remote subscribers.

### 9.4 Choosing between them

| Situation | Instruction |
|---|---|
| I need agent X's answer to proceed | `invoke` / `a2a` |
| I need a tool's result | `invoke` / `mcp` |
| Anyone interested should know this happened | `emit` / `a2a` |
| I need N responses from an unknown set | `emit`, then `wait` with `count: N` and `window` |

The last row composes the two: broadcast a request, then collect responses through the buffered topic layer.

---

## 10. Version migration

Migration moves in-flight instances from one definition version to another.

### 10.1 The problem

An instance is a program counter plus a heap: "instance 7 is paused at `/manager_approval`, holding these variables, with these timers, these subscriptions, and this compensation stack." Deploying version 4 raises five questions the engine cannot answer alone: where does the token land; was skipped work required; do the variables still typecheck; does the compensation stack still resolve; and what happens to timers and subscriptions.

Default behaviour is **pinning**: instances complete on the version they started. Migration is always explicit.

### 10.2 The migration plan artifact

Migration plans are first-class, versioned, validatable artifacts — never ad-hoc scripts.

```yaml
upir_migration: "0.1"
id: "invoice.approval/3→4"
from: { id: "invoice.approval", version: 3 }
to:   { id: "invoice.approval", version: 4 }

select:
  filter: "invoice.amount > decimal('1000')"   # CEL over instance variables
  # or: instances: ["inst-a1b2", "inst-c3d4"]
  # omitted = all instances on version 3

mappings:
  - from: "/manager_approval"
    to:   "/compliance_block/manager_approval"
  - from: "/wait_for_payment"
    to:   "/wait_for_payment"
  - from: "/legacy_review"
    action: complete_scope        # map | complete_scope | fail | suspend

variables:
  add:
    cost_centre: { type: string, default: "UNASSIGNED" }
  rename:
    - { from: "amt", to: "amount" }
  remove: ["legacy_flag"]
  transform:
    - target: "amount"
      expr: "decimal(string(amt))"

compensation:
  remap:
    - { from: "void_erp", to: "erp_void" }
    - { from: "legacy_undo", action: retain }   # keep the frozen v3 handler

timers:      preserve            # preserve | restart | recompute
subscriptions: preserve          # preserve | resubscribe
unmapped:    fail                # fail | suspend | complete_scope
plan_scopes: block               # block | complete | cancel

execution:
  mode: batch
  batch_size: 50
  on_instance_failure: quarantine  # quarantine | abort_batch
```

### 10.3 Mapping generation and stability

Instruction `id` is the stable key; the full **path** is the address. Because UPIR is block-structured, position is a path rather than a point in a free graph, so comparing two versions is a **tree diff** — tractable in a way that free-graph diffing is not.

The engine generates candidate mappings automatically from:

1. Identical paths in both versions.
2. Identical `id` at a different path (instruction moved into or out of a scope).
3. `previous_ids` declarations (§2.2) for renamed instructions.

Generated mappings are **proposed, never applied**. A human confirms or edits them. Inference is a convenience, not a semantic.

### 10.4 Validation

Two phases, both before anything moves.

**Static validation** — against the definition pair alone:
- every `from` path exists in the source version;
- every `to` path exists in the target version;
- variable transforms typecheck;
- compensation remappings resolve to instructions that exist in the target;
- capability sets do not widen.

**Instance validation** — dry-run against each selected instance:
- the instance's current paths are all covered by mappings or an `unmapped` policy;
- post-transform variables satisfy the target version's declared types;
- every entry on the compensation stack resolves under `compensation.remap`;
- no current path lies inside a `plan`-generated scope, unless `plan_scopes` permits.

Instances failing validation are reported before execution. A plan with `unmapped: fail` is rejected if any selected instance would be unmapped.

### 10.5 Execution

- Instances migrate **asynchronously in batches**, not in one transaction.
- Each instance transitions `stable → migrating → stable`. While `migrating` it accepts no events and dispatches no jobs; in-flight jobs are allowed to complete and are checkpointed before the move.
- Per-instance migration is a single transaction: rewrite paths, variables, compensation stack, timers and subscriptions together.
- Failures under `on_instance_failure: quarantine` mark the instance `quarantined` on the source version, leaving it operable, and continue the batch.
- A migration is recorded in `upir_history` per instance, with the plan id, so "why is this instance on version 4 when it started on version 3" is answerable.

### 10.6 Non-migratable scopes

Instances whose current path lies inside a `plan`-generated scope are **not migratable by default**. The scope's instructions were emitted at runtime by an agent and exist in no definition version, so no mapping can be authored against them.

Policies:

| `plan_scopes` | Behaviour |
|---|---|
| `block` (default) | Instance fails validation with `upir.migration_blocked`. |
| `complete` | Wait for the plan scope to exit, then migrate. Migration remains pending. |
| `cancel` | Cancel the plan scope (running its compensation), then migrate from the `plan` instruction itself. |

Because fragments are content-addressed (§4.11), the engine can identify plan-generated scopes positively rather than by heuristic.

### 10.7 Timers and subscriptions

| Policy | Effect |
|---|---|
| `timers: preserve` | Absolute fire time is retained. Default; correct for deadlines. |
| `timers: restart` | Duration timers restart from migration time. |
| `timers: recompute` | Re-evaluate the target version's timer expression against current variables. |
| `subscriptions: preserve` | Subscription id, correlation key and `since` are retained, so buffered events remain visible. Default. |
| `subscriptions: resubscribe` | Drop and recreate under the target version's declaration. Buffered events before migration are visible only if `from: buffered` and `since` still cover them. |

---

## 11. Multi-tenancy and capabilities

Instance state, history, definitions, topics and event buffers are tenant-scoped. Every instruction execution carries a tenant context; `principal` carries `tenant`; `concurrency_key` is implicitly tenant-namespaced.

`capabilities` may appear on the definition envelope, on `plan`, and on `call`. Semantics are **strictly narrowing** — a nested capability set is intersected with its parent, and a definition cannot grant itself instructions or targets its caller lacked.

| Field | Meaning |
|---|---|
| `allow` | Permitted opcodes. |
| `invoke_targets` | Glob patterns of permitted targets (covers workers, MCP tools, A2A agents and sandboxes uniformly). |
| `call_definitions` | Glob patterns of callable definitions. |
| `emit_topics` | Glob patterns of publishable topics. |
| `encodings` | Permitted opaque encodings. |
| `max_instructions`, `max_depth` | Size ceilings, primarily for `plan`. |

Violations raise `upir.capability_denied` — at compile time for static definitions, at splice time for `plan` fragments.

---

## 12. Instruction summary

| # | Instruction | Purpose |
|---|---|---|
| 1 | `invoke` | Call a worker, MCP tool, A2A agent, or sandboxed script |
| 2 | `set` | Pure state transformation |
| 3 | `switch` | N-way exclusive choice |
| 4 | `fork` | Parallel branches with join policy |
| 5 | `iterate` | Map over a collection, or loop while a condition holds |
| 6 | `wait` | Suspend for a duration, until a time, or for correlated events |
| 7 | `gate` | Human decision with assignment, quorum, deadline and escalation |
| 8 | `call` | Invoke another definition as a child instance |
| 9 | `emit` | Publish to a topic; no addressee |
| 10 | `end` | Terminate scope: success, typed failure, or terminate |
| 11 | `plan` | Agent emits a UPIR fragment, validated and spliced |

Cross-cutting fields on every instruction: `guard`, `retry`, `catch`, `timeout`, `compensate_with`, `idempotency_key`, `concurrency_key`, `in`, `out`, `metadata`.

---

## 13. Source notation mapping

Mappings for the executable subset of each source notation. Purely presentational elements are omitted.

### 13.1 Amazon States Language

| ASL | UPIR |
|---|---|
| `Task` | `invoke` |
| `Pass` | `set` |
| `Choice` (Choice Rules → `cases[].guard`, `Default` → `default`) | `switch` |
| `Parallel` | `fork`, `join: all` |
| `Map` (`MaxConcurrency` → `max_concurrency`) | `iterate`, `mode: parallel` |
| `Wait` (`Seconds` → `duration`, `Timestamp` → `until`) | `wait` |
| `Succeed` | `end`, `status: success` |
| `Fail` (`Error`/`Cause` → `error`) | `end`, `status: fail` |
| `Retry` | `retry` field |
| `Catch` | `catch` field |
| `Next` / `End` | implicit — UPIR bodies are ordered sequences |
| `InputPath` / `Parameters` / `ResultSelector` | `in` / `out` |

Clean. ASL is the closest existing IR to UPIR's shape; the only structural change is that explicit `Next` chaining becomes implicit sequencing. ASL has no compensation, human tasks or typed variables, so those UPIR features have no ASL source.

### 13.2 CNCF Serverless Workflow 1.0

| Serverless Workflow | UPIR |
|---|---|
| `call`, `run` | `invoke` |
| `set` | `set` |
| `switch` | `switch` |
| `fork` (`compete: true` → `join: any`) | `fork` |
| `for` | `iterate`, `mode: sequential` |
| `do` | scope nesting — no instruction |
| `wait` | `wait`, duration form |
| `listen` | `wait`, `subscribe` form |
| `emit` | `emit` |
| `raise` | `end`, `status: fail` |
| `try` / `catch` | `catch` field |
| retry policies | `retry` field |

Clean. `do` disappears because UPIR treats nesting as structure rather than as an instruction. Serverless Workflow lacks `decimal` and `principal`.

### 13.3 Argo Workflows

| Argo | UPIR |
|---|---|
| `container`, `script`, `resource` templates | `invoke` (target addresses the executor) |
| `dag` template | `fork` + guards, or a sequence when linear |
| `steps` template | ordered body; parallel step groups → `fork` |
| `suspend` | `gate` if human, `wait` if timed |
| `http` | `invoke` |
| `withItems` / `withParam` | `iterate`, `mode: parallel` |
| `when` | `guard` |
| `retryStrategy` | `retry` |
| `onExit` | `catch` on `*`, or `compensate_with` if semantically an undo |
| `continueOn` | `catch` with an empty body |
| artifacts | `ref`, kind `blob` |
| `templateRef` | `call` |

Argo DAG templates permit arbitrary dependency graphs. Non-series-parallel DAGs are not expressible in UPIR's block structure and MUST be rejected with a specific diagnostic. Most Argo DAGs in practice are series-parallel.

### 13.4 Flyte IDL

| Flyte | UPIR |
|---|---|
| `TaskNode` | `invoke` |
| `BranchNode` | `switch` |
| `WorkflowNode` (subworkflow / launchplan) | `call` |
| `ArrayNode` | `iterate`, `mode: parallel` |
| `GateNode` (approve) | `gate` |
| `GateNode` (signal) | `wait`, `subscribe` form |
| `GateNode` (sleep) | `wait`, duration form |
| retries | `retry` |
| `error` type | `error` |
| `blob`, `structured dataset` | `ref` + `opaque` encoding descriptor |
| `float` (scientific) | `opaque` with declared encoding |
| `float` (monetary) | `decimal` — **requires human judgement**; a compiler cannot distinguish a price from a probability |

The closest type mapping of any source. Flyte has no compensation and no `emit`.

### 13.5 Kubeflow PipelineSpec

| KFP | UPIR |
|---|---|
| executor task | `invoke` |
| DAG group | sequence or `fork` |
| `condition` group | `switch` |
| `ParallelFor` group | `iterate`, `mode: parallel` |
| `ExitHandler` | `catch` on `*` |
| `Importer` | `set` producing a `ref` |
| typed artifacts | `ref` + `opaque` encoding descriptor |

Same series-parallel caveat as Argo. No human tasks, events or compensation to translate.

### 13.6 BPMN 2.0 executable subset

The largest and least mechanical mapping.

| BPMN | UPIR |
|---|---|
| Service, Send, Script, Business Rule Task | `invoke` |
| User Task | `gate` |
| Manual Task | `gate` without a form |
| Receive Task | `wait`, `subscribe` form |
| Call Activity | `call` |
| Embedded Sub-Process | nested scope |
| Exclusive Gateway | `switch` |
| Parallel Gateway (split + join pair) | `fork`, `join: all` |
| Event-Based Gateway | `fork`, `join: any`, branches beginning with `wait` |
| Start Event (none) | first instruction of body |
| Start Event (message) | `wait` with `subscribe`, `from: buffered` |
| Start Event (timer) | `wait` with `cron`, or definition trigger metadata |
| End Event (none / error / terminate) | `end` with `success` / `fail` / `terminate` |
| Intermediate Timer Event | `wait` |
| Intermediate Message Catch | `wait`, `subscribe` form |
| Intermediate Message or Signal Throw | `emit`, `scope: tenant` |
| Boundary Error Event | `catch` on the attached instruction |
| Boundary Timer Event (interrupting) | `timeout` + `catch` on `upir.timeout` |
| Boundary Timer Event (non-interrupting) | `fork`, `join: any`, with a `wait` branch |
| Boundary Message Event | `catch`, or `fork` when non-interrupting |
| Compensation Event + Handler | `compensate_with` |
| Transaction Sub-Process | nested scope with `compensate_with` on members |
| Event Sub-Process (error) | `catch` at scope level |
| Multi-Instance (sequential / parallel) | `iterate` with corresponding `mode` |
| Ad-Hoc Sub-Process | `plan` |
| Escalation Events | typed error in `catch` |

**Does not translate:**

| BPMN construct | Reason |
|---|---|
| Inclusive (OR) Gateway | Join semantics ambiguous. Compile to `fork` + guards where branch conditions are independent; reject otherwise. |
| Complex Gateway | No semantics worth preserving. |
| Unstructured cycles | Not representable in block structure. Reject with diagnostic. |
| Cross-boundary sequence flows | Same. |
| Link Events | An artifact of large-diagram layout, not semantics. Resolve during parsing. |
| Lanes / Pools | Presentational and organisational. Lanes map onto `gate.assign_to`; pools become separate definitions coordinating via `emit` / `wait`. |
| Data Objects / Data Stores | Variables and `ref` respectively. |

A BPMN compiler therefore accepts a strict subset of legal BPMN. This is intentional and SHOULD be surfaced as live validation in the modeller, not discovered at deploy time. Structural conversion is the Refined Process Structure Tree decomposition; fragments classified as rigid are exactly those to reject.

### 13.7 Zeebe executable model

Zeebe already implements a restricted BPMN subset whose restrictions largely coincide with UPIR's. The mapping is §13.6 minus complex gateways and transaction sub-processes, which Zeebe also omits. Zeebe job workers map onto `invoke` targets; Zeebe message correlation keys map onto `subscribe.correlate_on`.

### 13.8 Windmill `FlowValue`

| Windmill module | UPIR |
|---|---|
| `rawscript` | `invoke`, `target_kind: sandbox` |
| `script` (hub / workspace path) | `invoke` |
| `flow` | `call` |
| `forloopflow` | `iterate`, mode from the `parallel` flag |
| `whileloopflow` | `iterate`, `while` form |
| `branchone` | `switch` |
| `branchall` | `fork`, `join: all` (or sequential `iterate` when `parallel: false`) |
| `identity` | `set` with no assignments |
| `suspend` | `gate` |
| `sleep` | `wait` |
| `retry` | `retry` |
| `failure_module` | `catch` on `*` |
| `stop_after_if` | guarded `end` |
| `skip_if` | `guard` |
| `cache_ttl` | `invoke.cache` |
| `mock` | `metadata`, test-only |

Clean — Windmill's module tree is already block-structured. Note that `rawscript` becoming `invoke` is where the capability model bites: inline user code is a target subject to `invoke_targets`, not a privileged instruction.

### 13.9 Activepieces flows

| Activepieces | UPIR |
|---|---|
| trigger (webhook) | definition trigger metadata |
| trigger (polling / scheduled) | `wait` with `cron` as first instruction |
| trigger (app event) | `wait` with `subscribe`, `from: buffered` |
| piece action step | `invoke` (target addresses the piece) |
| code step | `invoke`, `target_kind: sandbox` |
| router / branch step | `switch` |
| loop-on-items step | `iterate`, `mode: sequential` |
| approval piece | `gate` |
| delay piece | `wait`, duration form |
| connections / auth | `ref`, kind `secret` |

Clean. Activepieces flows are linear with nested branches and loops, so they are structurally a subset of UPIR. Piece steps retain their TypeScript implementation and execute on a Node worker; the orchestrator does not distinguish them from any other `invoke` target.

### 13.10 jBPM Process Virtual Machine

| PVM | UPIR |
|---|---|
| `Node` + pluggable `Activity` | `invoke`, or the appropriate control instruction |
| `Transition` | implicit sequencing / `switch` cases |
| Decision node | `switch` |
| Fork / Join nodes | `fork` |
| State node | `wait` |
| Task node | `gate` |
| Sub-process node | `call` |
| Exception handlers | `catch` |
| Event listeners | `metadata` hooks, or `emit` |

Conceptually the closest ancestor — PVM was built for exactly UPIR's purpose, one execution core behind multiple front-end notations. It differs in being a free graph, untyped, and lacking compensation and agent planning.

### 13.11 Temporal command set

Temporal is imperative code rather than a declarative IR, so this is a lowering rather than a translation. Included as a completeness check on the instruction vocabulary.

| Temporal command | UPIR |
|---|---|
| `ScheduleActivityTask` | `invoke` |
| `StartTimer` | `wait`, duration form |
| `StartChildWorkflowExecution` | `call` |
| `SignalExternalWorkflowExecution` | `emit` |
| `RequestCancelActivityTask` | cancellation (§5.5) |
| `RecordMarker` | `set` |
| `UpsertSearchAttributes` | `metadata` |
| `CompleteWorkflowExecution` | `end`, `status: success` |
| `FailWorkflowExecution` | `end`, `status: fail` |
| `ContinueAsNew` | no equivalent — history compaction is a storage concern, not an instruction |
| retry policies | `retry` |
| saga by convention | `compensate_with` — UPIR makes explicit what Temporal leaves to library convention |

### 13.12 Agent protocols

| Source | UPIR |
|---|---|
| MCP tool call | `invoke`, `target_kind: tool`, `protocol: mcp` |
| MCP resource read | `invoke`, `target_kind: tool`, returning a `ref` |
| A2A message send (request/response) | `invoke`, `target_kind: agent`, `protocol: a2a` |
| A2A broadcast | `emit`, `transport: a2a`, `scope: external` |
| A2A task artifact | `ref` + `opaque` encoding descriptor |

---

## 14. Open questions for v0.2

1. **Workflow pattern conformance.** Formal assessment against van der Aalst's control-flow patterns (workflowpatterns.com) has not been performed. Deliberately-unsupported patterns should be enumerated. *Deferred by decision.*
2. **Topic schema evolution.** §6.1 declares payload types; changing them while events sit in the buffer is unspecified.
3. **Cross-tenant `emit`.** `scope: external` crosses a trust boundary. Authentication, authorisation and payload redaction rules are unspecified.
4. **`list<T>` spill semantics.** The read path for a spilled list (§3.3) — whether downstream instructions see a `ref` or a transparently-rehydrated array — is unspecified.
5. **CEL cost budgets.** Concrete limits per expression context have not been set, and they interact with `iterate` guard evaluation at scale.
6. **Migration of `iterate` mid-flight.** An instance partway through a parallel iteration with a partially-filled `collect` has no specified mapping when the body changes.
7. **Encoding registry governance.** Who may register an encoding, and how encoding version deprecation interacts with pinned definitions.
