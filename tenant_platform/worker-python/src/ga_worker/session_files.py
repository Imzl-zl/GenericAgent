"""Per-session file sandbox and artifact persistence helpers."""

from __future__ import annotations

import hashlib
import os
import re
import secrets
import stat
from pathlib import Path

FILE_MARKER_RE = re.compile(r"\[FILE:([^\]]+)\]")

SESSION_FILES_DIR = "session_files"
ATTACHMENTS_DIR = "attachments"
OUTPUTS_DIR = "outputs"
MAX_DOCUMENT_ARTIFACT_BYTES = 8 * 1024 * 1024
_SHA256_RE = re.compile(r"^[a-f0-9]{64}$")


def session_key_digest(session_key: str) -> str:
    return hashlib.sha256((session_key or "").encode("utf-8")).hexdigest()


def session_sandbox_root(runtime_root: Path, session_key: str) -> Path:
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


def persist_document_artifact(root: Path, file_name: str, content: bytes, sha256: str) -> str:
    root = Path(root).resolve()
    if not isinstance(content, bytes) or not 0 < len(content) <= MAX_DOCUMENT_ARTIFACT_BYTES:
        raise ValueError("document artifact size is invalid")
    if not isinstance(sha256, str) or not _SHA256_RE.fullmatch(sha256) or hashlib.sha256(content).hexdigest() != sha256:
        raise ValueError("document artifact digest is invalid")
    _validate_document_artifact_name(file_name)
    outputs = root / OUTPUTS_DIR
    if os.name != "nt":
        return _persist_document_artifact_posix(root, file_name, content, sha256)
    try:
        info = outputs.lstat()
    except OSError as exc:
        raise ValueError("session outputs directory is unavailable") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode) or outputs.resolve() != outputs:
        raise ValueError("session outputs directory is invalid")

    stem = Path(file_name).stem
    suffix = Path(file_name).suffix
    candidates = [file_name, f"{stem}-{sha256[:12]}{suffix}"]
    for candidate_name in candidates:
        candidate = outputs / candidate_name
        if candidate.exists() or candidate.is_symlink():
            if _existing_artifact_matches(candidate, content, sha256):
                return f"{OUTPUTS_DIR}/{candidate_name}"
            continue
        if _write_artifact_no_replace(outputs, candidate_name, content):
            return f"{OUTPUTS_DIR}/{candidate_name}"
        if _existing_artifact_matches(candidate, content, sha256):
            return f"{OUTPUTS_DIR}/{candidate_name}"
    raise ValueError("document artifact output name conflicts with an existing file")


def _persist_document_artifact_posix(root: Path, file_name: str, content: bytes, sha256: str) -> str:
    directory_flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    try:
        root_fd = os.open(root, directory_flags)
    except OSError as exc:
        raise ValueError("session output root is unavailable") from exc
    try:
        root_info = os.fstat(root_fd)
        path_info = root.lstat()
        if not _same_directory(root_info, path_info):
            raise ValueError("session output root changed during artifact write")
        try:
            outputs_fd = os.open(OUTPUTS_DIR, directory_flags, dir_fd=root_fd)
        except OSError as exc:
            raise ValueError("session outputs directory is unavailable") from exc
        try:
            outputs_info = os.fstat(outputs_fd)
            relative_info = os.stat(OUTPUTS_DIR, dir_fd=root_fd, follow_symlinks=False)
            if not _same_directory(outputs_info, relative_info):
                raise ValueError("session outputs directory is invalid")
            stem = Path(file_name).stem
            suffix = Path(file_name).suffix
            for candidate_name in (file_name, f"{stem}-{sha256[:12]}{suffix}"):
                if _existing_artifact_matches_at(outputs_fd, candidate_name, content, sha256):
                    result = f"{OUTPUTS_DIR}/{candidate_name}"
                elif _write_artifact_no_replace_at(outputs_fd, candidate_name, content):
                    result = f"{OUTPUTS_DIR}/{candidate_name}"
                elif _existing_artifact_matches_at(outputs_fd, candidate_name, content, sha256):
                    result = f"{OUTPUTS_DIR}/{candidate_name}"
                else:
                    continue
                if not _directory_handle_still_reachable(root, root_fd, OUTPUTS_DIR, outputs_fd):
                    raise ValueError("session outputs directory changed during artifact write")
                return result
        finally:
            os.close(outputs_fd)
    finally:
        os.close(root_fd)
    raise ValueError("document artifact output name conflicts with an existing file")


def _same_directory(left: os.stat_result, right: os.stat_result) -> bool:
    return (
        stat.S_ISDIR(left.st_mode)
        and stat.S_ISDIR(right.st_mode)
        and left.st_dev == right.st_dev
        and left.st_ino == right.st_ino
    )


def _directory_handle_still_reachable(root: Path, root_fd: int, name: str, directory_fd: int) -> bool:
    try:
        return _same_directory(os.fstat(root_fd), root.lstat()) and _same_directory(
            os.fstat(directory_fd),
            os.stat(name, dir_fd=root_fd, follow_symlinks=False),
        )
    except OSError:
        return False


def _existing_artifact_matches_at(directory_fd: int, file_name: str, content: bytes, sha256: str) -> bool:
    try:
        fd = os.open(file_name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=directory_fd)
    except OSError:
        return False
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size != len(content):
            return False
        digest = hashlib.sha256()
        while chunk := os.read(fd, 64 * 1024):
            digest.update(chunk)
        return digest.hexdigest() == sha256
    except OSError:
        return False
    finally:
        os.close(fd)


def _write_artifact_no_replace_at(directory_fd: int, file_name: str, content: bytes) -> bool:
    temp_name = f".ga-document-{secrets.token_hex(16)}.tmp"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
    fd = os.open(temp_name, flags, 0o640, dir_fd=directory_fd)
    try:
        view = memoryview(content)
        while view:
            written = os.write(fd, view)
            if written <= 0:
                raise OSError("document artifact write made no progress")
            view = view[written:]
        os.fsync(fd)
    finally:
        os.close(fd)
    try:
        try:
            os.link(
                temp_name,
                file_name,
                src_dir_fd=directory_fd,
                dst_dir_fd=directory_fd,
                follow_symlinks=False,
            )
        except FileExistsError:
            return False
        os.fsync(directory_fd)
        return True
    finally:
        try:
            os.unlink(temp_name, dir_fd=directory_fd)
        except FileNotFoundError:
            pass


def _validate_document_artifact_name(file_name: str) -> None:
    if (
        not isinstance(file_name, str)
        or not file_name.strip()
        or file_name != file_name.strip()
        or len(file_name.encode("utf-8")) > 255
        or file_name in {".", ".."}
        or "/" in file_name
        or "\\" in file_name
        or any(ord(char) < 32 or ord(char) == 127 for char in file_name)
        or not file_name.lower().endswith(".docx")
    ):
        raise ValueError("document artifact file name is invalid")


def _existing_artifact_matches(path: Path, content: bytes, sha256: str) -> bool:
    try:
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_size != len(content):
            return False
        return hashlib.sha256(path.read_bytes()).hexdigest() == sha256
    except OSError:
        return False


def _write_artifact_no_replace(outputs: Path, file_name: str, content: bytes) -> bool:
    temp_name = f".ga-document-{secrets.token_hex(16)}.tmp"
    temp_path = outputs / temp_name
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(temp_path, flags, 0o640)
    linked = False
    try:
        with os.fdopen(fd, "wb", closefd=True) as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        try:
            os.link(temp_path, outputs / file_name)
            linked = True
        except FileExistsError:
            return False
        if os.name != "nt":
            directory_fd = os.open(outputs, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        return True
    finally:
        try:
            temp_path.unlink()
        except FileNotFoundError:
            pass
        if not linked and temp_path.exists():
            temp_path.unlink(missing_ok=True)


def read_text_file(path: Path, *, max_bytes: int = 1024 * 1024) -> str:
    if max_bytes <= 0:
        raise ValueError("text file byte limit must be positive")
    with Path(path).open("rb") as handle:
        raw = handle.read(max_bytes + 1)
    if len(raw) > max_bytes:
        raise ValueError(f"text file exceeds {max_bytes} bytes")
    for enc in ('utf-8', 'utf-8-sig', 'utf-16'):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode('utf-8', errors='replace')


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
