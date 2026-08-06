"""Worker SOP 安装工具: 经 Platform Sophub proxy 下载 SOP 到工作区 memory/sops/.

方案 §5.2: SOPHub 使用部署管理员维护的平台账号; Runner 只能通过 Platform
受控 proxy 搜索/下载, 不能获得 Sophub API Key。安装目标为当前工作区
memory/sops/<remote-sop-id>.md; 再次下载不静默覆盖已被用户修改的本地 SOP。
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import requests

from ga_worker.credential_config import CredentialConfigError, load_runtime_document

# 覆盖保护: 本地 SOP 若与最近一次安装内容不一致, 视为用户已修改。
SOP_INSTALL_MARKER_SUFFIX = ".sophub-installed"


class SopToolError(ValueError):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


@dataclass(frozen=True)
class RuntimeSophubProxy:
    base_url: str
    capability_token: str


def load_runtime_sophub_proxy(config_root: Path) -> RuntimeSophubProxy | None:
    """从签名 runtime config 读取 Sophub proxy capability。"""
    root = Path(config_root)
    try:
        _, document = load_runtime_document(root)
    except (CredentialConfigError, OSError, ValueError) as exc:
        raise SopToolError("SOPHUB_CONFIG_ERROR", f"cannot load runtime sophub config: {exc}") from exc
    raw = document.get("_platform_sophub")
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise SopToolError("SOPHUB_CONFIG_ERROR", "_platform_sophub must be an object")
    unknown = set(raw) - {"base_url", "capability_token"}
    if unknown:
        raise SopToolError("SOPHUB_CONFIG_ERROR", f"unknown fields: {sorted(unknown)}")
    base_url = str(raw.get("base_url") or "").strip().rstrip("/")
    token = str(raw.get("capability_token") or "").strip()
    if not base_url or not token:
        raise SopToolError("SOPHUB_CONFIG_ERROR", "base_url and capability_token are required")
    return RuntimeSophubProxy(base_url=base_url, capability_token=token)


def sops_dir(workspace_memory: Path) -> Path:
    """当前工作区 memory/sops/; 不存在时创建。"""
    d = Path(workspace_memory) / "sops"
    d.mkdir(parents=True, exist_ok=True)
    return d


def _installed_digest_path(sops: Path, remote_id: str) -> Path:
    return sops / (f"{remote_id}.md" + SOP_INSTALL_MARKER_SUFFIX)


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _is_user_modified(sops: Path, remote_id: str, content: str) -> bool:
    """本地 SOP 存在且与最近安装内容不同 → 用户已修改, 不得静默覆盖。"""
    target = sops / f"{remote_id}.md"
    if not target.is_file():
        return False
    marker = _installed_digest_path(sops, remote_id)
    if marker.is_file():
        last_installed = marker.read_text(encoding="utf-8").strip()
        if last_installed == _sha256(target):
            return False  # 与上次安装一致, 可更新
    return True  # 无安装记录或内容被改过


def install_sop_to_workspace(
    proxy: RuntimeSophubProxy,
    workspace_memory: Path,
    remote_id: str,
    content: str,
    *,
    timeout_seconds: float = 30.0,
) -> Path:
    """下载内容写入工作区 memory/sops/<remote-id>.md(不覆盖已修改)。

    直接由 Worker 调用时 proxy 已代发请求; 本函数只负责安全落盘。
    返回写入的路径。
    """
    if not remote_id or any(ch in remote_id for ch in "/\\\x00"):
        raise SopToolError("INVALID_SOP_ID", f"invalid remote sop id: {remote_id!r}")
    if not isinstance(content, str) or len(content.encode("utf-8")) > 64 * 1024:
        raise SopToolError("INVALID_SOP_CONTENT", "sop content must be markdown <= 64KiB")
    sops = sops_dir(workspace_memory)

    if _is_user_modified(sops, remote_id, content):
        raise SopToolError(
            "SOP_LOCAL_MODIFIED",
            f"sop {remote_id} was modified locally; refusing to overwrite",
        )

    target = sops / f"{remote_id}.md"
    # 原子写: 临时文件 + rename, 避免半成品。
    tmp = target.with_suffix(".md.tmp")
    tmp.write_text(content, encoding="utf-8")
    tmp.replace(target)
    _installed_digest_path(sops, remote_id).write_text(_sha256(target), encoding="utf-8")
    return target


def sophub_search(proxy: RuntimeSophubProxy, query: str, *, timeout_seconds: float = 30.0) -> dict[str, Any]:
    """经 Platform proxy 搜索 SOP(仅公开 approved single-file markdown)。"""
    if not query.strip():
        raise SopToolError("INVALID_QUERY", "query is required")
    try:
        resp = requests.get(
            f"{proxy.base_url}/v1/worker/sophub/search",
            params={"q": query.strip()},
            headers={"Authorization": "Bearer " + proxy.capability_token},
            timeout=timeout_seconds,
        )
    except requests.RequestException as exc:
        raise SopToolError("SOPHUB_NETWORK_ERROR", f"sophub search failed: {exc}") from exc
    if resp.status_code != 200:
        raise SopToolError("SOPHUB_REJECTED", f"sophub search rejected: {resp.status_code}")
    return resp.json()


def sophub_install(proxy: RuntimeSophubProxy, workspace_memory: Path, remote_id: str, *, timeout_seconds: float = 30.0) -> Path:
    """经 Platform proxy 下载并安装 SOP 到工作区。"""
    if not remote_id or any(ch in remote_id for ch in "/\\\x00"):
        raise SopToolError("INVALID_SOP_ID", f"invalid remote sop id: {remote_id!r}")
    try:
        resp = requests.get(
            f"{proxy.base_url}/v1/worker/sophub/install",
            params={"id": remote_id},
            headers={"Authorization": "Bearer " + proxy.capability_token},
            timeout=timeout_seconds,
        )
    except requests.RequestException as exc:
        raise SopToolError("SOPHUB_NETWORK_ERROR", f"sophub install failed: {exc}") from exc
    if resp.status_code != 200:
        raise SopToolError("SOPHUB_REJECTED", f"sophub install rejected: {resp.status_code}")
    payload = resp.json()
    content = payload.get("content")
    if not isinstance(content, str):
        raise SopToolError("INVALID_SOP_CONTENT", "install response missing content")
    return install_sop_to_workspace(proxy, workspace_memory, remote_id, content)
