"""Immutable runtime overlay materialization under GA_RUNTIME_DIR/<session-id>/legacy-overlay."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import stat
from pathlib import Path
from typing import Any

LEGACY_MODULES = (
    "agentmain.py",
    "ga.py",
    "llmcore.py",
    "agent_loop.py",
    "simphtml.py",
)
LEGACY_PLUGINS = (
    "plugins/__init__.py",
    "plugins/hooks.py",
)
LEGACY_ASSETS = (
    "assets/tools_schema.json",
    "assets/tools_schema_cn.json",
    "assets/sys_prompt.txt",
    "assets/sys_prompt_en.txt",
    "assets/global_mem_insight_template.txt",
    "assets/global_mem_insight_template_en.txt",
    "assets/insight_fixed_structure.txt",
    "assets/insight_fixed_structure_en.txt",
    "assets/code_run_header.py",
)

OVERLAY_MANIFEST_ENTRIES: tuple[str, ...] = LEGACY_MODULES + LEGACY_PLUGINS + LEGACY_ASSETS

_SESSION_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


class OverlayError(ValueError):
    """Raised when overlay materialization is refused."""


def encode_session_id(session_key: str) -> str:
    """Opaque fixed encoding of a validated session identity (never raw path fragment)."""
    if not session_key or not isinstance(session_key, str):
        raise OverlayError("session_key must be a non-empty string")
    if "\x00" in session_key:
        raise OverlayError("session_key contains NUL")
    digest = hashlib.sha256(session_key.encode("utf-8")).hexdigest()[:32]
    return f"s-{digest}"


def _link_dir(link: Path, target: Path) -> None:
    """创建目录链接: 优先 symlink; Windows 无特权时回退 junction。"""
    try:
        link.symlink_to(target, target_is_directory=True)
    except (OSError, NotImplementedError):
        if os.name == "nt":
            import subprocess

            subprocess.run(
                ["cmd", "/c", "mklink", "/J", str(link), str(target)],
                check=True,
                capture_output=True,
            )
        else:
            raise


def _validate_session_id(session_id: str) -> str:
    if not session_id or not isinstance(session_id, str):
        raise OverlayError("session_id must be a non-empty string")
    if not _SESSION_ID_RE.match(session_id):
        raise OverlayError(f"invalid session_id: {session_id!r}")
    if any(sep in session_id for sep in ("/", "\\", "..")):
        raise OverlayError(f"session_id path escape: {session_id!r}")
    return session_id


def _resolve_under(root: Path, *parts: str) -> Path:
    root = root.resolve()
    candidate = root.joinpath(*parts).resolve()
    try:
        candidate.relative_to(root)
    except ValueError as exc:
        raise OverlayError(f"path escapes runtime root: {candidate}") from exc
    return candidate


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def _make_readonly(path: Path) -> None:
    mode = path.stat().st_mode
    path.chmod(mode & ~stat.S_IWRITE & ~stat.S_IWGRP & ~stat.S_IWOTH)


def _copy_manifest_entry(src_root: Path, dest_root: Path, rel: str) -> str:
    src = (src_root / rel).resolve()
    try:
        src.relative_to(src_root.resolve())
    except ValueError as exc:
        raise OverlayError(f"source escapes legacy root: {rel}") from exc
    if not src.is_file():
        raise OverlayError(f"missing overlay source: {rel}")
    # Symlinks allowed only when resolved target is the same immutable file under legacy root.
    if src.is_symlink():
        real = src.resolve()
        try:
            real.relative_to(src_root.resolve())
        except ValueError as exc:
            raise OverlayError(f"symlink escapes legacy root: {rel}") from exc
        if not real.is_file():
            raise OverlayError(f"symlink target not a file: {rel}")
        src = real
    dest = dest_root / rel
    dest.parent.mkdir(parents=True, exist_ok=True)
    expected = _sha256_file(src)
    try:
        with src.open("rb") as source, dest.open("xb") as target:
            shutil.copyfileobj(source, target, length=1024 * 1024)
            target.flush()
            os.fsync(target.fileno())
    except Exception:
        dest.unlink(missing_ok=True)
        raise
    digest = _sha256_file(dest)
    if digest != expected:
        dest.unlink(missing_ok=True)
        raise OverlayError(f"SHA-256 mismatch after copy: {rel}")
    _make_readonly(dest)
    return digest


def materialize_runtime_overlay(
    *,
    legacy_root: Path,
    runtime_root: Path,
    session_id: str,
    lang: str = "zh",
) -> tuple[Path, dict[str, Any]]:
    """
    Materialize immutable overlay at runtime_root/<session_id>/legacy-overlay.
    Returns (overlay_dir, manifest_dict).
    """
    legacy_root = Path(legacy_root)
    runtime_root = Path(runtime_root)
    if not legacy_root.is_dir():
        raise OverlayError(f"legacy_root missing: {legacy_root}")
    if not (legacy_root / "agentmain.py").is_file():
        raise OverlayError(f"agentmain.py missing under legacy_root: {legacy_root}")
    if not runtime_root.is_dir():
        raise OverlayError(f"runtime_root missing: {runtime_root}")

    session_id = _validate_session_id(session_id)
    session_dir = _resolve_under(runtime_root, session_id)
    overlay_dir = _resolve_under(runtime_root, session_id, "legacy-overlay")

    if overlay_dir.exists():
        # Collision: only reuse if complete matching manifest is present.
        existing = overlay_dir / "OVERLAY_MANIFEST.json"
        if not existing.is_file():
            raise OverlayError(f"overlay collision without manifest: {overlay_dir}")
        manifest = json.loads(existing.read_text(encoding="utf-8"))
        if set(manifest.get("entries", [])) != set(OVERLAY_MANIFEST_ENTRIES):
            raise OverlayError("overlay collision with mismatched manifest")
        return overlay_dir, manifest

    session_dir.mkdir(parents=True, exist_ok=True)
    overlay_dir.mkdir(parents=True, exist_ok=False)

    digests: dict[str, str] = {}
    for rel in OVERLAY_MANIFEST_ENTRIES:
        digests[rel] = _copy_manifest_entry(legacy_root, overlay_dir, rel)

    # Writable per-session memory/temp (not in immutable manifest).
    # 方案 §4/§5: 容器 Runner 中 memory/temp 由 Manager 以工作区 subpath 挂载到
    # 固定位置(/ga/legacy/memory、/ga/legacy/temp), overlay 通过符号链接指向
    # 挂载点, GA 保持原生 ./temp 与 ../memory 相对路径约定且直接读写工作区。
    # 非容器(loopback 开发)环境仍创建本地目录。
    workspace_mem_raw = os.environ.get("GA_WORKSPACE_MEMORY", "").strip()
    workspace_temp_raw = os.environ.get("GA_WORKSPACE_TEMP", "").strip()
    workspace_mem = Path(workspace_mem_raw) if workspace_mem_raw else None
    workspace_temp = Path(workspace_temp_raw) if workspace_temp_raw else None

    mem_dir = overlay_dir / "memory"
    if workspace_mem is not None and workspace_mem.is_dir():
        if mem_dir.exists():
            shutil.rmtree(mem_dir)
        _link_dir(mem_dir, workspace_mem)
    else:
        mem_dir.mkdir(parents=True, exist_ok=True)
        lang_suffix = "_en" if lang == "en" else ""
        mem_txt = mem_dir / "global_mem.txt"
        if not mem_txt.exists():
            mem_txt.write_text("# [Global Memory - L2]\n", encoding="utf-8")
        mem_insight = mem_dir / "global_mem_insight.txt"
        if not mem_insight.exists():
            template = overlay_dir / f"assets/global_mem_insight_template{lang_suffix}.txt"
            text = template.read_text(encoding="utf-8") if template.is_file() else ""
            mem_insight.write_text(text, encoding="utf-8")
    temp_dir = overlay_dir / "temp"
    if workspace_temp is not None and workspace_temp.is_dir():
        if temp_dir.exists():
            shutil.rmtree(temp_dir)
        _link_dir(temp_dir, workspace_temp)
    else:
        temp_dir.mkdir(parents=True, exist_ok=True)
        (temp_dir / "model_responses").mkdir(parents=True, exist_ok=True)

    manifest: dict[str, Any] = {
        "schema_version": "genericagent.overlay-manifest.v1",
        "session_id": session_id,
        "entries": list(OVERLAY_MANIFEST_ENTRIES),
        "sha256": digests,
        "legacy_root": str(legacy_root.resolve()),
    }
    manifest_path = overlay_dir / "OVERLAY_MANIFEST.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    _make_readonly(manifest_path)
    return overlay_dir, manifest
