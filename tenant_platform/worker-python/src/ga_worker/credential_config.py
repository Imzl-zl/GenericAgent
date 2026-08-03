"""Validate session-scoped GA runtime credential metadata."""

from __future__ import annotations

import hashlib
import json
import hmac
from dataclasses import dataclass
from pathlib import Path
from typing import Any

CHECKSUM_PLACEHOLDER = "0" * 64
RUNTIME_CONFIG_FILENAME = "mykey.runtime.json"


class CredentialConfigError(ValueError):
    pass


@dataclass(frozen=True)
class RuntimeCredentialMetadata:
    generation: int
    checksum: str
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
    generation = metadata.get("credential_generation")
    checksum = metadata.get("config_checksum")
    snapshot_id = metadata.get("routing_snapshot_id")
    if not isinstance(generation, int) or isinstance(generation, bool) or generation <= 0:
        raise CredentialConfigError("credential_generation must be a positive integer")
    if not isinstance(checksum, str) or len(checksum) != 64:
        raise CredentialConfigError("config_checksum must be 64 hexadecimal characters")
    try:
        bytes.fromhex(checksum)
    except ValueError as exc:
        raise CredentialConfigError("config_checksum must be hexadecimal") from exc
    if not isinstance(snapshot_id, str) or not snapshot_id:
        raise CredentialConfigError("routing_snapshot_id is required")
    jtis_raw = metadata.get("jtis")
    jtis: frozenset[str] = frozenset()
    if jtis_raw is not None:
        if not isinstance(jtis_raw, list) or not all(isinstance(j, str) and j for j in jtis_raw):
            raise CredentialConfigError("jtis must be a list of non-empty strings")
        jtis = frozenset(jtis_raw)

    checksum_bytes = checksum.encode("ascii")
    if encoded.count(checksum_bytes) != 1:
        raise CredentialConfigError("config_checksum must occur exactly once in runtime JSON")
    expected = hashlib.sha256(encoded.replace(checksum_bytes, CHECKSUM_PLACEHOLDER.encode("ascii"), 1)).hexdigest()
    if not hmac.compare_digest(expected, checksum):
        raise CredentialConfigError("CONFIG_CHECKSUM_MISMATCH")
    return RuntimeCredentialMetadata(generation, checksum, snapshot_id, jtis), document


def load_runtime_metadata(config_root: Path) -> RuntimeCredentialMetadata:
    metadata, _ = load_runtime_document(config_root)
    return metadata


def validate_reload_request(
    metadata: RuntimeCredentialMetadata,
    credential_generation: int,
    config_checksum: str,
) -> None:
    if credential_generation <= 0:
        raise CredentialConfigError("credential_generation must be positive")
    if metadata.generation != credential_generation:
        raise CredentialConfigError(
            f"credential generation mismatch: file={metadata.generation} request={credential_generation}"
        )
    if not hmac.compare_digest(metadata.checksum, config_checksum):
        raise CredentialConfigError("CONFIG_CHECKSUM_MISMATCH")
