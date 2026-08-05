"""Validate session-scoped GA runtime credential metadata.

决策 D1: 凭证热刷新协议已删除——credential generation 与 config checksum
校验随 ReloadCredentials RPC 一起移除。metadata 仅保留 routing_snapshot_id
与 JTIs(任务边界 capability 校验用)。
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

RUNTIME_CONFIG_FILENAME = "mykey.runtime.json"


class CredentialConfigError(ValueError):
    pass


@dataclass(frozen=True)
class RuntimeCredentialMetadata:
    routing_snapshot_id: str
    # JTIs 是当前凭证集签发的全部 capability token JTI(方案 §7):
    # task_runner 校验 ExecuteTask 的 capability_jti 必须属于该集合。
    jtis: frozenset[str] = frozenset()


def load_runtime_document(config_root: Path) -> tuple[RuntimeCredentialMetadata, dict[str, Any]]:
    path = Path(config_root) / RUNTIME_CONFIG_FILENAME
    try:
        encoded = path.read_bytes()
        document: dict[str, Any] = json.loads(encoded)
    except (OSError, json.JSONDecodeError) as exc:
        raise CredentialConfigError(f"cannot read {path}: {exc}") from exc

    metadata = document.get("_platform_runtime")
    if not isinstance(metadata, dict):
        raise CredentialConfigError("_platform_runtime metadata is required")
    snapshot_id = metadata.get("routing_snapshot_id")
    if not isinstance(snapshot_id, str) or not snapshot_id:
        raise CredentialConfigError("routing_snapshot_id is required")
    jtis_raw = metadata.get("jtis")
    jtis: frozenset[str] = frozenset()
    if jtis_raw is not None:
        if not isinstance(jtis_raw, list) or not all(isinstance(j, str) and j for j in jtis_raw):
            raise CredentialConfigError("jtis must be a list of non-empty strings")
        jtis = frozenset(jtis_raw)
    return RuntimeCredentialMetadata(snapshot_id, jtis), document


def load_runtime_metadata(config_root: Path) -> RuntimeCredentialMetadata:
    metadata, _ = load_runtime_document(config_root)
    return metadata
