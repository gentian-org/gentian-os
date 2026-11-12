# Dispatcher API and the run contract

What a component that starts runs calls, what a run's pod is told, and what
the component answers. The concept is in
[concepts/control-plane.md §4.8](../concepts/control-plane.md); what a run may
reach is in [concepts/networking.md §2.1](../concepts/networking.md).

## 1. The caller

A component whose profile declares `requires.services.runs` is given the
Secret `runs-<component>` in its namespace:

| Entry | Is |
| --- | --- |
| `RUNS_URL` | the dispatcher's address inside the cluster |
| `RUNS_KEY` | the component's key, sent as `Authorization: Bearer <key>` |

The key says which tenant and which component is calling. A request without a
key the operator's table names is answered `401`. The dispatcher is reached
from inside the cluster only, and only by such a component.

## 2. Calls

| Call | Does | Answers |
| --- | --- | --- |
| `POST /v1/runs` | starts a run | `201` with the run; `200` with the run that exists when the id was used before |
| `GET /v1/runs/{id}` | reads a run | `200`; `404` once its pod is gone |
| `GET /v1/runs` | the caller's runs that still have a pod | `200` `{"runs": [...]}` |
| `GET /v1/runs/{id}/logs` | the end of the run's log, at most 64 KiB | `200` as text |
| `DELETE /v1/runs/{id}` | cancels: deletes the run's pods | `204` |
| `GET /v1/actions` | what the caller may run | `200` `{"actions": [...]}` |

A run to start:

```json
{
  "id": "invoice-7/step-3",
  "action": "ocr",
  "deadlineSeconds": 60,
  "callbackToken": "…",
  "person": "ada@example.org"
}
```

| Field | Is |
| --- | --- |
| `id` | the caller's own id for the run, at most 253 characters without a slash. Asking twice with one id starts one run |
| `action` | an action installed in the caller's tenant, or the name of one of the caller's own runs, which is looked for first |
| `deadlineSeconds` | optional; at most what the action declares, which is the default |
| `callbackToken` | a credential the caller made for this run. The run presents it on every call back (§4) |
| `person` | optional; who the run is for. It is the caller's claim: the dispatcher records it and verifies nothing |

A run as it is told:

```json
{"id": "invoice-7/step-3", "name": "ocr", "state": "working", "attempt": 1,
 "startedAt": "…", "finishedAt": "…", "exitCode": 0, "reason": ""}
```

`state` is `working`, `completed` or `failed`; `reason` says why a run failed
(`DeadlineExceeded`, `Error`, `OOMKilled`). A finished run's pod is kept ten
minutes, and after that the caller's own record is what there is.

Refusals:

| Status | When |
| --- | --- |
| `401` | no key the table names |
| `404` | nothing of that name is installed in the tenant or declared by the caller; or no such run |
| `422` | no id or token, or a deadline longer than the action's |
| `429` | the caller has as many runs under way as `concurrency` allows, or the quota of the namespace for runs is full |
| `503` | the tenant's namespace for runs is not there yet |

The platform makes one attempt per run. `attempt` is always `1`; a caller
that wants a step run again starts a run with a new id.

## 3. What a run's pod is

One pod in `tenant-<t>-runs`, deleted ten minutes after it ended:

- the action's image at its pinned build, as user 65532, under the
  `restricted` pod security standard, with the action's `resources` as both
  request and limit;
- the ServiceAccount `run`, which holds no right and mounts no token;
- `activeDeadlineSeconds` set to the run's deadline, and no restart;
- the platform's run program as the first process, around the action's
  `command`.

Its environment:

| Variable | Is |
| --- | --- |
| `RUN_ID` | the caller's id for the run |
| `RUN_NAME` | what runs |
| `RUN_ATTEMPT` | which attempt this is |
| `RUN_CALLBACK_URL` | the caller's callback address: its Service, port and path |
| `RUN_TOKEN` | the `callbackToken` of the start |

`RUN_TOKEN` is in the pod's specification, which the operator and the
dispatcher can read. A caller makes it for one run and stops accepting it when
the run ends.

## 4. The contract of a command

The action's `command` reads the run's inputs on standard input and writes its
outputs to standard output. What it writes to standard error is the run's log.
Its exit code is the run's: `0` is `completed`.

The run program stands between the command and the caller, and calls the
caller twice, each time with `Authorization: Bearer $RUN_TOKEN`:

| Call | When | Carries |
| --- | --- | --- |
| `GET $RUN_CALLBACK_URL/runs/$RUN_ID/inputs` | before the command starts | the answer's body becomes the command's standard input |
| `POST $RUN_CALLBACK_URL/runs/$RUN_ID/outputs` | after the command ended | the command's standard output, at most 16 MiB, with `X-Run-Exit-Code` and `X-Run-Attempt` |

A caller that refuses the first call ends the run before the command starts.

A command that wants more (progress, a file of evidence, a person's answer, a
call to an adapter) asks the caller itself, at `RUN_CALLBACK_URL` with
`RUN_TOKEN`: the variables are the command's too. What paths a caller offers
beyond the two above is the caller's own interface.
