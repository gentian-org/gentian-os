from __future__ import annotations

import os
import re
import tempfile
from pathlib import Path

from git import Repo

from app.core.config import get_settings


class GitOpsError(Exception):
    pass


class DeploymentsGitOps:
  def __init__(self) -> None:
    settings = get_settings()
    self._repo_url = settings.gentian_deployments_repo
    self._work_path = settings.gentian_deployments_path
    if not self._repo_url and not self._work_path:
      raise GitOpsError("GENTIAN_DEPLOYMENTS_REPO or GENTIAN_DEPLOYMENTS_PATH required")

  def _repo(self) -> Repo:
    if self._work_path and Path(self._work_path, ".git").exists():
      repo = Repo(self._work_path)
      repo.remotes.origin.pull(rebase=True)
      return repo
    if not self._repo_url:
      raise GitOpsError("Deployments git repository not configured")
    tmp = tempfile.mkdtemp(prefix="gentian-deployments-")
    self._work_path = tmp
    return Repo.clone_from(self._repo_url, tmp)

  def _tenant_file(self, tenant: str) -> Path:
    assert self._work_path
    root = Path(self._work_path)
    candidates = list(root.glob(f"**/tenants/{tenant}.yaml"))
    if not candidates:
      for path in root.glob("**/tenants/*.yaml"):
        text = path.read_text(encoding="utf-8")
        if f"name: {tenant}" in text and "kind: Tenant" in text:
          candidates.append(path)
    if not candidates:
      raise GitOpsError(f"Tenant file for '{tenant}' not found")
    return candidates[0]

  def install_app(self, tenant: str, profile: str, actor: str) -> str:
    repo = self._repo()
    tenant_path = self._tenant_file(tenant)
    content = tenant_path.read_text(encoding="utf-8")
    if re.search(rf"^\s+profile:\s+{re.escape(profile)}\s*$", content, re.MULTILINE):
      return "already_installed"
    if re.search(r"^  apps:\s*$", content, re.MULTILINE):
      content = re.sub(
        r"(^  apps:\s*$)",
        rf"\1\n  - profile: {profile}",
        content,
        count=1,
        flags=re.MULTILINE,
      )
    else:
      content = content.rstrip() + f"\n  apps:\n  - profile: {profile}\n"
    tenant_path.write_text(content, encoding="utf-8")
    repo.index.add([str(tenant_path)])
    if not repo.is_dirty():
      return "no_change"
    commit = repo.index.commit(f"feat({tenant}): install {profile} (via app-store by {actor})")
    repo.remotes.origin.push()
    return commit.hexsha

  def uninstall_app(self, tenant: str, profile: str, actor: str) -> str:
    repo = self._repo()
    tenant_path = self._tenant_file(tenant)
    content = tenant_path.read_text(encoding="utf-8")
    new_content, count = re.subn(
      rf"^  - profile: {re.escape(profile)}\s*\n",
      "",
      content,
      flags=re.MULTILINE,
    )
    if count == 0:
      return "not_installed"
    tenant_path.write_text(new_content, encoding="utf-8")
    repo.index.add([str(tenant_path)])
    commit = repo.index.commit(f"feat({tenant}): uninstall {profile} (via app-store by {actor})")
    repo.remotes.origin.push()
    return commit.hexsha

  def list_installed_profiles(self, tenant: str) -> list[str]:
    tenant_path = self._tenant_file(tenant)
    content = tenant_path.read_text(encoding="utf-8")
    return re.findall(r"^\s+profile:\s+(\S+)\s*$", content, re.MULTILINE)
