# Gentian Deployments Restructure Plan

## Goals

- Move from stage-aggregator tenant sync to tenant-directory sync.
- Keep configuration cluster-first and reduce drift between installer env and GitOps state.
- Add a second cluster baseline (`clusters/test`) for validation and migration dry runs.

## Target State

- Tenant sync model uses ArgoCD `ApplicationSet` with git directory generator:
  - `clusters/<cluster>/tenants/*/<stage>`
- Cluster-scoped non-secret settings live in:
  - `clusters/<cluster>/kernel/cluster-settings.env`
- Installer/update auto-loads cluster settings from `gentian-deployments` before prompting.

## Migration Steps

1. Snapshot current state:
   - `git status` in both `gentian-os` and `gentian-deployments`.
   - Export running Argo objects:
     - `kubectl get applications,applicationsets -n argocd -o yaml > /tmp/argocd-before.yaml`
2. Remove stage aggregators:
   - Delete `clusters/<cluster>/tenants/stages`.
3. Convert tenant sync:
   - Replace `gentian-tenants` Argo `Application` with `ApplicationSet` in bootstrap templates/manifests.
4. Add cluster settings files:
   - `clusters/<cluster>/kernel/cluster-settings.env`.
5. Wire installer/update to load cluster settings from deployments repo.
6. Validate with a second cluster tree (`clusters/test`) before production rollout.

## Rollback Plan

1. Revert to previous commit in `gentian-os` and `gentian-deployments`.
2. Re-apply old Argo manifests:
   - `kubectl apply -f /tmp/argocd-before.yaml`
3. Re-run `./update.sh --argocd --crossplane`.
4. Verify `gentian-tenants` returns to single `Application` model.

## Validation Checklist

1. Path integrity:
   - `find clusters -maxdepth 5 -type f | sort`
2. Template integrity:
   - `grep -R "tenants/stages" -n .` should return no active references.
3. Argo render placeholders:
   - Ensure `%CLUSTER%` and `%STAGE%` are replaced in rendered bootstrap manifests.
4. AppSet behavior:
   - `kubectl get applicationsets -n argocd`
   - `kubectl get applications -n argocd | grep gentian-tenants`
5. Cluster settings load path:
   - Confirm installer log shows loading:
     - `clusters/<cluster>/kernel/cluster-settings.env`

## Known Failure Points

- `ApplicationSet` CRD missing or outdated in Argo installation.
- Invalid git generator path (cluster/stage typo).
- Local install env values unexpectedly overriding cluster settings.
- Tenant directory without `kustomization.yaml` causing Argo sync errors.
- Cluster settings file accidentally containing secrets.

## Rules for Cluster Settings Files

- Allowed: non-secret topology and behavior values (domain, networking mode, service endpoints, storage class, mail mode).
- Forbidden: credentials, API tokens, passwords, private keys.
- Secrets stay in `install.secrets.env`, OpenBao, or external secret backends.
