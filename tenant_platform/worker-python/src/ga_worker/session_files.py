"""Per-session file sandbox and artifact persistence helpers."""

from __future__ import annotations

import hashlib
import os
import re
from pathlib import Path

FILE_MARKER_RE = re.compile(r"\[FILE:([^\]]+)\]")

SESSION_FILES_DIR = "session_files"
ATTACHMENTS_DIR = "attachments"
OUTPUTS_DIR = "outputs"


def session_key_digest(session_key: str) -> str:
    return hashlib.sha256((session_key or "").encode("utf-8")).hexdigest()


def session_sandbox_root(runtime_root: Path, session_key: str) -> Path:
    # 方案 §6: 附件/输出统一到工作区 temp/。容器 Runner 中 GA_WORKSPACE_TEMP
    # 指向挂载的工作区 temp, 且 GA handler.cwd 解析到同一目录(审查: 恢复
    # 原生相对路径语义, 不再用 session_files/<digest> 中间层)。非容器(loopback
    # 开发)回退 runtime_root/session_files/<digest>。
    workspace_temp = os.environ.get("GA_WORKSPACE_TEMP", "").strip()
    if workspace_temp:
        return Path(workspace_temp)
    return Path(runtime_root) / SESSION_FILES_DIR / session_key_digest(session_key)


def ensure_session_sandbox(runtime_root: Path, session_key: str) -> Path:
    root = session_sandbox_root(runtime_root, session_key)
    (root / ATTACHMENTS_DIR).mkdir(parents=True, exist_ok=True)
    (root / OUTPUTS_DIR).mkdir(parents=True, exist_ok=True)
    return root


def resolve_under_root(root: Path, raw_path: str) -> Path:
    root = Path(root).resolve()
    if not raw_path:
        raise ValueError("path is required")
    candidate = Path(raw_path)
    if not candidate.is_absolute():
        candidate = root / candidate
    resolved = candidate.resolve()
    try:
        resolved.relative_to(root)
    except ValueError as exc:
        raise ValueError(f"path escapes session sandbox: {raw_path}") from exc
    return resolved


def normalize_output_name(name: str, default: str = "document.docx") -> str:
    raw = (name or "").strip().replace("\\", "/")
    parts = [part for part in raw.split("/") if part not in ("", ".")]
    safe_parts: list[str] = []
    for part in parts:
        if part == "..":
            continue
        safe = re.sub(r'[:*?"<>|]+', '_', part)
        safe = safe.strip()
        if safe:
            safe_parts.append(safe)
    default_name = re.sub(r'[:*?"<>|]+', '_', (default or "document.docx").split("/")[-1]) or "document.docx"
    if not safe_parts:
        safe_parts = [OUTPUTS_DIR, default_name]
    elif safe_parts[0] != OUTPUTS_DIR:
        safe_parts = [OUTPUTS_DIR, safe_parts[-1]]
    elif len(safe_parts) == 1:
        safe_parts.append(default_name)
    if not safe_parts[-1].lower().endswith('.docx'):
        safe_parts[-1] += '.docx'
    return "/".join(safe_parts)


def append_missing_file_markers(body: str, rel_paths: list[str]) -> str:
    normalized_existing = {
        match.group(1).strip().replace("\\", "/")
        for match in FILE_MARKER_RE.finditer(body or "")
    }
    missing: list[str] = []
    for rel_path in rel_paths or []:
        normalized = (rel_path or "").strip().replace("\\", "/")
        if not normalized or normalized in normalized_existing:
            continue
        missing.append(normalized)
    if not missing:
        return body
    suffix = "\n".join(f"[FILE:{path}]" for path in missing)
    base = (body or "").rstrip()
    if not base:
        return suffix + "\n"
    return base + "\n\n" + suffix + "\n"
