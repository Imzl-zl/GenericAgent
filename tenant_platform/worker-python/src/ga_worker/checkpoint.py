"""Bounded snapshot bundle encode/decode for quiescent task boundaries."""

from __future__ import annotations

import hashlib
import json
import os
import stat
import tempfile
from pathlib import Path
from typing import Any

SNAPSHOT_SCHEMA_VERSION = "genericagent.snapshot.v1"
RESULT_CONTENT_TYPE = "text/plain; charset=utf-8"


class CheckpointError(ValueError):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


def result_digest_for(body: str) -> str:
    return "sha256:" + hashlib.sha256(body.encode("utf-8")).hexdigest()


def _utf8_size(obj: Any) -> int:
    return len(json.dumps(obj, ensure_ascii=False, separators=(",", ":")).encode("utf-8"))


def _check_history_budget(backend_history: Any, agent_history: Any, max_history_bytes: int) -> None:
    size = _utf8_size(backend_history) + _utf8_size(agent_history)
    if size > max_history_bytes:
        raise CheckpointError(
            "HISTORY_LIMIT_EXCEEDED",
            f"history exceeds max_history_bytes ({size} > {max_history_bytes})",
        )


def _trim_history_to_budget(backend_history: list[Any], agent_history: list[Any], max_history_bytes: int) -> None:
    """成对裁剪最旧消息直到总大小不超限(审查 F9)。

    backend_history 与 agent_history 是同一对话的两个视图(一一对应),
    每次从头部成对移除一条, 直到 size <= max_history_bytes 或列表耗尽。
    成功任务不得因历史增长被改判失败——确定性裁剪(保留最近消息)优于
    整任务报错; 裁剪后的开头可能是单边残留(agent_history 先耗尽), 由
    模型容忍。注意: 就地修改调用方列表(会话已结束, 无共享引用风险)。
    """
    if max_history_bytes <= 0:
        return
    while (backend_history or agent_history) and (
        _utf8_size(backend_history) + _utf8_size(agent_history) > max_history_bytes
    ):
        if backend_history:
            backend_history.pop(0)
        if agent_history:
            agent_history.pop(0)


def _check_working_budget(working: Any, max_working_bytes: int) -> None:
    size = _utf8_size(working)
    if size > max_working_bytes:
        raise CheckpointError(
            "WORKING_LIMIT_EXCEEDED",
            f"working exceeds max_working_bytes ({size} > {max_working_bytes})",
        )


def build_snapshot_bundle(
    *,
    task_id: str,
    session_key: str,
    backend_history: list[Any],
    agent_history: list[Any],
    working: dict[str, Any],
    display_history: list[Any],
    result_body: str,
    max_history_bytes: int,
    max_working_bytes: int,
    runner_generation: int = 0,
) -> dict[str, Any]:
    if not isinstance(result_body, str):
        raise CheckpointError("INVALID_RESULT", "result body must be str")
    # 审查 F9: history 超限确定性裁剪(保留最近消息), 不再整任务失败;
    # working 为结构化状态, 静默裁剪会丢失语义, 超限仍然显式报错。
    _trim_history_to_budget(backend_history, agent_history, max_history_bytes)
    _check_working_budget(working, max_working_bytes)
    body = result_body
    digest = result_digest_for(body)
    bundle = {
        "schema_version": SNAPSHOT_SCHEMA_VERSION,
        "task_id": task_id,
        "session_key": session_key,
        "backend_history": backend_history,
        "agent_history": agent_history,
        "working": working,
        "display_history": display_history,
        "result": {
            "content_type": RESULT_CONTENT_TYPE,
            "body": body,
        },
        "result_digest": digest,
    }
    if runner_generation:
        bundle["runner_generation"] = runner_generation
    return bundle


def bundle_checksum_bytes(raw: bytes) -> str:
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def encode_bundle_bytes(bundle: dict[str, Any]) -> bytes:
    return json.dumps(bundle, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode("utf-8")


def load_snapshot_bundle(
    path: Path,
    *,
    expected_checksum: str,
    max_history_bytes: int,
    max_working_bytes: int,
    max_bundle_bytes: int = 0,
) -> dict[str, Any]:
    path = Path(path)
    # 审查 R5-I5: 单次打开 + 同一 fd 校验/限读——旧实现的
    # path.stat() + path.read_bytes() 是两次路径解析: 攻击者可在 stat 后把
    # 文件替换成超大文件或持续扩展同一 inode, 造成无界内存读取(checksum
    # 校验在读完之后, 不能先读后查限)。O_NOFOLLOW 拒绝最后组件符号链接
    # (committed/ 位于 Runner 可写挂载), fstat 校验普通文件, 读取上限为
    # max_bundle_bytes + 1(超限立即拒绝, 不读入内存)。
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(path, flags)
    except OSError as exc:
        raise CheckpointError("SNAPSHOT_NOT_FOUND", f"snapshot open failed: {path}") from exc
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            raise CheckpointError(
                "SNAPSHOT_NOT_FOUND", f"snapshot is not a regular file: {path}"
            )
        limit = max_bundle_bytes if max_bundle_bytes > 0 else None
        if limit is not None and st.st_size > limit:
            raise CheckpointError(
                "SNAPSHOT_TOO_LARGE",
                f"snapshot size {st.st_size} exceeds max_bundle_bytes {limit}",
            )
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = os.read(fd, 64 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if limit is not None and total > limit:
                raise CheckpointError(
                    "SNAPSHOT_TOO_LARGE",
                    f"snapshot exceeds max_bundle_bytes {limit} while reading",
                )
            chunks.append(chunk)
        raw = b"".join(chunks)
    except CheckpointError:
        raise
    except OSError as exc:
        raise CheckpointError("SNAPSHOT_NOT_FOUND", f"snapshot read failed: {path}") from exc
    finally:
        os.close(fd)
    actual = bundle_checksum_bytes(raw)
    if actual != expected_checksum:
        raise CheckpointError(
            "SNAPSHOT_CHECKSUM_MISMATCH",
            f"snapshot checksum mismatch: expected {expected_checksum}, got {actual}",
        )
    try:
        data = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise CheckpointError("SNAPSHOT_INVALID", f"invalid snapshot JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise CheckpointError("SNAPSHOT_INVALID", "snapshot root must be object")
    if data.get("schema_version") != SNAPSHOT_SCHEMA_VERSION:
        raise CheckpointError(
            "SNAPSHOT_INVALID",
            f"unsupported snapshot schema_version: {data.get('schema_version')!r}",
        )
    backend_history = data.get("backend_history") or []
    agent_history = data.get("agent_history") or []
    working = data.get("working") or {}
    if not isinstance(backend_history, list) or not isinstance(agent_history, list):
        raise CheckpointError("SNAPSHOT_INVALID", "history fields must be lists")
    if not isinstance(working, dict):
        raise CheckpointError("SNAPSHOT_INVALID", "working must be an object")
    try:
        _check_history_budget(backend_history, agent_history, max_history_bytes)
        _check_working_budget(working, max_working_bytes)
    except CheckpointError as exc:
        # Surface as snapshot/policy limit for restore path.
        raise CheckpointError(exc.code, exc.message) from exc
    result = data.get("result") or {}
    if not isinstance(result, dict) or "body" not in result:
        raise CheckpointError("SNAPSHOT_INVALID", "result.body required")
    body = result["body"]
    if not isinstance(body, str):
        raise CheckpointError("SNAPSHOT_INVALID", "result.body must be str")
    expected_rd = data.get("result_digest")
    actual_rd = result_digest_for(body)
    if expected_rd and expected_rd != actual_rd:
        raise CheckpointError("SNAPSHOT_INVALID", "result_digest does not match body")
    return data


def write_checkpoint_atomic(
    *,
    staging_ref: Path,
    bundle: dict[str, Any],
    max_bundle_bytes: int,
    token: str,
) -> tuple[str, str]:
    """
    Write bundle to staging_ref via token-scoped temp + fsync + atomic rename.
    Returns (bundle_checksum, result_digest).
    """
    staging_ref = Path(staging_ref)
    raw = encode_bundle_bytes(bundle)
    if max_bundle_bytes and len(raw) > max_bundle_bytes:
        raise CheckpointError(
            "BUNDLE_TOO_LARGE",
            f"bundle size {len(raw)} exceeds max_bundle_bytes {max_bundle_bytes}",
        )
    result_digest = bundle["result_digest"]
    checksum = bundle_checksum_bytes(raw)
    staging_ref.parent.mkdir(parents=True, exist_ok=True)
    # Token-scoped temporary file in same directory for atomic rename.
    safe_token = hashlib.sha256(token.encode("utf-8")).hexdigest()[:16]
    fd, tmp_name = tempfile.mkstemp(
        prefix=f".ckpt-{safe_token}-",
        suffix=".tmp",
        dir=str(staging_ref.parent),
    )
    tmp_path = Path(tmp_name)
    try:
        # mkstemp 默认 0600, Platform(共享组)必须能读 staging bundle
        # (审查: 共享卷权限协议; 0640 = 属主读写 + 共享组读)。
        os.fchmod(fd, 0o640)
        with os.fdopen(fd, "wb") as f:
            f.write(raw)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp_path, staging_ref)
        # fsync directory to persist the rename (crash durability, 审查 I8).
        # Windows may not support opening a directory for fsync; only swallow
        # the specific "not supported" cases there, never on POSIX.
        try:
            dir_fd = os.open(str(staging_ref.parent), os.O_RDONLY)
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
        except OSError as exc:
            if os.name == "nt":
                # Windows: 目录 fsync 不受支持/无 POSIX 语义, 容忍。
                pass
            else:
                raise CheckpointError(
                    "STAGING_DIR_FSYNC_FAILED",
                    f"fsync staging dir failed: {exc}",
                ) from exc
    except Exception:
        if tmp_path.exists():
            try:
                tmp_path.unlink()
            except OSError:
                pass
        raise
    return checksum, result_digest
