#!/usr/bin/env python3
# =============================================================================
# scripts/tests/llm_models.py — the models the gateway offers are the Cluster
# claim's.
#
# Nothing registers a model at the gateway: its model list is its
# configuration file, which its chart writes from the claim's spec.llm, and
# the claim reaches the chart as a values file of the LLM ApplicationSet.
# Rendered with helm, no cluster.
#
#   llm_models.py claim      the ApplicationSet hands the chart the claim
#   llm_models.py models     the configuration file, against a claim
#   llm_models.py tokens     a provider's token: where it is and is not
#   llm_models.py gateway    the gateway reads the file, and only the file
# =============================================================================
import sys
import tempfile

import yaml

from llm_network_policies import application, render
from store_network_policies import ROOT, Failure, appsets

# A claim the schema accepts (crossplane/tests/unit/schema/valid), with an
# instance and two providers.
CLAIM = "crossplane/tests/unit/schema/valid/cluster-llm-models.yaml"
# What the gateway offers for it, by name: the model it calls, and where.
OFFERED = {
    "qwen-qwen2.5-7b-instruct": ("openai/Qwen/Qwen2.5-7B-Instruct", "http://vllm-qwen-inference.{ns}.svc.cluster.local:8000/v1"),
    "infomaniak/gemma-4-31b": ("openai/google/gemma-4-31B-it", "https://api.infomaniak.com/2/ai/12345/openai/v1"),
    "infomaniak/bge-m3": ("openai/BAAI/bge-m3", "https://api.infomaniak.com/2/ai/12345/openai/v1"),
}
CONFIG_MAP = "litellm-config"
PROVIDER_SECRET = "llm-provider-credentials"
KEY_VARIABLE = "LLM_PROVIDER_KEY_"


def one(docs, kind, name):
    found = [d for d in docs if d.get("kind") == kind and d["metadata"]["name"] == name]
    if len(found) != 1:
        raise Failure(f"the chart renders {len(found)} {kind} named {name}, want one")
    return found[0]


def config(docs):
    return yaml.safe_load(one(docs, "ConfigMap", CONFIG_MAP)["data"]["config.yaml"])


def gateway(docs):
    deployment = one(docs, "Deployment", "litellm-proxy")
    return deployment, deployment["spec"]["template"]["spec"]["containers"][0]


def with_claim(text):
    """The chart, rendered for a claim written here."""
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        f.write(text)
        f.flush()
        return render(application(), "-f", f.name)


def check_claim():
    cluster, repo, revision = "c1", "https://git.example/deployments", "main"
    sets = [d for d in appsets("--set-string", "llmEnabled=true", "--set-string", f"deploymentsCluster={cluster}",
                               "--set-string", f"deploymentsRepo={repo}", "--set-string", f"deploymentsRevision={revision}")
            if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == "gentian-llm"]
    if len(sets) != 1:
        raise Failure("no gentian-llm ApplicationSet rendered for a cluster that serves models")
    sources = sets[0]["spec"]["template"]["spec"]["sources"]
    reference = [s for s in sources if "ref" in s]
    chart = [s for s in sources if "path" in s]
    if len(reference) != 1 or len(chart) != 1:
        raise Failure(f"the ApplicationSet's sources are not one chart and one reference: {sources}")
    reference = reference[0]
    if reference["repoURL"] != repo or reference["targetRevision"] != revision:
        raise Failure(f"the claim is not read from the cluster's deployments repository at its revision: {reference}")
    # The same file the claims Application applies.
    want = f"${reference['ref']}/clusters/{cluster}/kernel/claims/cluster.yaml"
    if chart[0]["helm"].get("valueFiles") != [want]:
        raise Failure(f"the chart is handed {chart[0]['helm'].get('valueFiles')}, want the Cluster claim, [{want!r}]")
    applied = [d for d in appsets("--set-string", f"deploymentsCluster={cluster}")
               if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == "gentian-claims"]
    if len(applied) != 1 or applied[0]["spec"]["template"]["spec"]["source"]["path"] != f"clusters/{cluster}/kernel/claims":
        raise Failure("the claims ApplicationSet no longer applies clusters/<cluster>/kernel/claims, where the chart reads the claim")
    # A parameter would win over the claim: none may name what the claim says.
    for name in application()["params"]:
        if name.startswith("spec."):
            raise Failure(f"the ApplicationSet passes {name} as a parameter, over the claim")


def check_models():
    app = application()
    ns = app["namespace"]
    models = config(render(app, "-f", CLAIM))["model_list"]
    got = {m["model_name"]: (m["litellm_params"]["model"], m["litellm_params"]["api_base"]) for m in models}
    want = {name: (model, base.format(ns=ns)) for name, (model, base) in OFFERED.items()}
    if got != want or len(models) != len(want):
        raise Failure(f"for {CLAIM} the gateway offers {got}, want {want}")
    info = {m["model_name"]: m.get("model_info") or {} for m in models}
    if info["infomaniak/gemma-4-31b"] != {"mode": "chat", "max_tokens": 8192}:
        raise Failure(f"a chat model with maxTokens is described as {info['infomaniak/gemma-4-31b']}")
    if info["infomaniak/bge-m3"] != {"mode": "embedding"}:
        raise Failure(f"an embedding model is described as {info['infomaniak/bge-m3']}")

    # Instances are read only where the claim says the cluster has GPUs.
    no_gpu = config(render(app, "-f", CLAIM, "--set", "spec.llm.gpuAcceleration=false"))["model_list"]
    if sorted(m["model_name"] for m in no_gpu) != ["infomaniak/bge-m3", "infomaniak/gemma-4-31b"]:
        raise Failure(f"without GPUs the gateway offers {[m['model_name'] for m in no_gpu]}")

    # A claim that names no model: an empty list, and a gateway that starts.
    for text in ("spec:\n  llm:\n    enabled: true\n", "spec:\n  kernelDomain: example.org\n"):
        if config(with_claim(text)) != {"model_list": []}:
            raise Failure(f"a claim without models gives the configuration {config(with_claim(text))}")
    # The mock model server is not a model.
    if config(render(app)) != {"model_list": []}:
        raise Failure("the chart's own defaults offer a model")

    # Removing an entry removes its model; changing the list replaces the pods.
    full = render(app, "-f", CLAIM)
    less = render(app, "-f", CLAIM, "--set", "spec.llm.providers=null")
    if [m["model_name"] for m in config(less)["model_list"]] != ["qwen-qwen2.5-7b-instruct"]:
        raise Failure("with the providers removed from the claim their models are still offered")
    sums = [gateway(d)[0]["spec"]["template"]["metadata"]["annotations"]["checksum/config"] for d in (full, less)]
    if sums[0] == sums[1]:
        raise Failure("a claim with other models leaves the gateway's pods as they are: the checksum did not change")
    same = render(app, "-f", CLAIM, "--set", "spec.kernelDomain=other.example")
    if gateway(same)[0]["spec"]["template"]["metadata"]["annotations"]["checksum/config"] != sums[0]:
        raise Failure("a change of the claim that is not about models replaces the gateway's pods")


def check_tokens():
    app = application()
    docs = render(app, "-f", CLAIM)
    claim = yaml.safe_load((ROOT / CLAIM).read_text())
    properties = {p["name"]: p["apiKeyProperty"] for p in claim["spec"]["llm"]["providers"]}
    for m in config(docs)["model_list"]:
        key = m["litellm_params"]["api_key"]
        provider = m["model_name"].split("/")[0] if "/" in m["model_name"] else None
        if provider is None:
            continue
        if key != f"os.environ/{KEY_VARIABLE}{properties[provider]}":
            raise Failure(f"{m['model_name']} takes its key from {key!r}, want the variable of {properties[provider]}")
    # The variable is set from the Secret the ExternalSecret writes, mounted
    # where the container's command and its probe read it.
    _, container = gateway(docs)
    deployment = one(docs, "Deployment", "litellm-proxy")
    volumes = {v["name"]: v for v in deployment["spec"]["template"]["spec"]["volumes"]}
    mounts = {m["name"]: m["mountPath"] for m in container["volumeMounts"]}
    secret = volumes["provider-keys"]["secret"]
    if secret["secretName"] != PROVIDER_SECRET or secret.get("optional") is not True:
        raise Failure(f"the providers' tokens are mounted from {secret}: want {PROVIDER_SECRET}, optional")
    start = container["command"][-1]
    probe = container["livenessProbe"]["exec"]["command"][-1]
    for what, text in (("command", start), ("probe", probe)):
        if mounts["provider-keys"] not in text:
            raise Failure(f"the gateway's {what} does not read {mounts['provider-keys']}, where the tokens are mounted")
    if f'export "{KEY_VARIABLE}${{name}}=' not in start:
        raise Failure(f"the gateway's command no longer sets {KEY_VARIABLE}<property> from the mounted Secret")
    external = one(docs, "ExternalSecret", PROVIDER_SECRET)
    if external["spec"]["target"]["name"] != PROVIDER_SECRET:
        raise Failure("the ExternalSecret for the providers' tokens writes another Secret than the gateway mounts")
    # No provider on the claim, no ExternalSecret: its vault path was never
    # written, and it would report an error for good.
    for extra in ((), ("--set", "spec.llm.providers=null")):
        left = [d for d in render(app, *(("-f", CLAIM) if extra else ()), *extra)
                if d.get("kind") == "ExternalSecret" and d["metadata"]["name"] == PROVIDER_SECRET]
        if left:
            raise Failure("the ExternalSecret for provider tokens is rendered for a claim that names no provider")


def check_gateway():
    docs = render(application(), "-f", CLAIM)
    deployment, container = gateway(docs)
    volumes = {v["name"]: v for v in deployment["spec"]["template"]["spec"]["volumes"]}
    mounts = {m["name"]: m["mountPath"] for m in container["volumeMounts"]}
    if volumes["config"]["configMap"]["name"] != CONFIG_MAP:
        raise Failure(f"the gateway mounts {volumes['config']} as its configuration, not {CONFIG_MAP}")
    if f"--config {mounts['config']}/config.yaml" not in container["command"][-1]:
        raise Failure("the gateway is not started with the configuration file it mounts")
    # Models from the file alone: none loaded from, none stored in, the database.
    env = {e["name"]: e.get("value") for e in container["env"]}
    if env.get("STORE_MODEL_IN_DB") != "False":
        raise Failure(f"STORE_MODEL_IN_DB is {env.get('STORE_MODEL_IN_DB')!r}: a model could be added beside the claim's")


def main():
    checks = {"claim": check_claim, "models": check_models, "tokens": check_tokens, "gateway": check_gateway}
    args = sys.argv[1:]
    if len(args) != 1 or args[0] not in checks:
        print(f"usage: {sys.argv[0]} {'|'.join(checks)}", file=sys.stderr)
        return 2
    try:
        checks[args[0]]()
    except Failure as e:
        print(f"        {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
