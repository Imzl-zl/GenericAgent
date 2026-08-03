"""checkpoint 预算与裁剪行为测试(审查 F9)。"""

import hashlib
import json
import os

import pytest

from ga_worker.checkpoint import (
    CheckpointError,
    _trim_history_to_budget,
    build_snapshot_bundle,
    load_snapshot_bundle,
)
from pathlib import Path


def _entry(text: str) -> dict:
    return {"role": "user", "content": text}


def test_trim_history_removes_oldest_pairs() -> None:
    backend = [_entry(f"b{i}") for i in range(20)]
    agent = [_entry(f"a{i}") for i in range(20)]
    # 预算小到只能容纳最后几条。
    _trim_history_to_budget(backend, agent, 200)
    assert len(backend) == len(agent) > 0
    # 保留的是最新消息(裁剪从头部开始)。
    assert backend[-1]["content"] == "b19"
    assert agent[-1]["content"] == "a19"
    total = len(json.dumps(backend, ensure_ascii=False, separators=(",", ":"))) + len(
        json.dumps(agent, ensure_ascii=False, separators=(",", ":"))
    )
    assert total <= 200


def test_trim_history_zero_or_negative_budget_is_noop() -> None:
    backend = [_entry("x"), _entry("y")]
    agent = [_entry("x"), _entry("y")]
    _trim_history_to_budget(backend, agent, 0)
    assert len(backend) == 2
    _trim_history_to_budget(backend, agent, -1)
    assert len(backend) == 2


def test_trim_history_handles_empty_side() -> None:
    backend = [_entry("b0")]
    agent: list = []
    _trim_history_to_budget(backend, agent, 1)
    # 单边列表耗尽后另一侧继续裁剪, 不抛错。
    assert isinstance(backend, list)
    assert isinstance(agent, list)


def test_build_snapshot_bundle_trims_history_instead_of_failing() -> None:
    backend = [_entry(f"backend-{i}-" + "x" * 64) for i in range(50)]
    agent = [_entry(f"agent-{i}-" + "x" * 64) for i in range(50)]
    bundle = build_snapshot_bundle(
        task_id="t1",
        session_key="personal:1",
        backend_history=backend,
        agent_history=agent,
        working={"k": "v"},
        display_history=[],
        result_body="ok",
        max_history_bytes=512,
        max_working_bytes=1024,
        runner_generation=1,
    )
    # 裁剪后写入的 bundle 必须满足预算(且没有抛错)。
    raw = json.dumps(bundle, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode("utf-8")
    assert len(raw) < 4096
    assert bundle["result"]["body"] == "ok"


def test_build_snapshot_bundle_working_over_budget_still_fails() -> None:
    # 审查 F9: working 超限保持显式报错(结构化状态不可静默裁剪)。
    with pytest.raises(CheckpointError) as exc:
        build_snapshot_bundle(
            task_id="t1",
            session_key="personal:1",
            backend_history=[],
            agent_history=[],
            working={"big": "x" * 4096},
            display_history=[],
            result_body="ok",
            max_history_bytes=1024,
            max_working_bytes=64,
        )
    assert exc.value.code == "WORKING_LIMIT_EXCEEDED"


def test_load_snapshot_bundle_restore_path_keeps_strict_budget(tmp_path) -> None:
    # 恢复路径是完整性校验: 超限 checkpoint 必须报错, 不允许静默裁剪
    # (数据来自 committed/ 共享卷, 超限说明异常, 裁剪会掩盖问题)。
    from ga_worker.checkpoint import encode_bundle_bytes

    bundle = {
        "schema_version": "genericagent.snapshot.v1",
        "task_id": "t1",
        "session_key": "personal:1",
        "backend_history": [_entry("x" * 512)],
        "agent_history": [],
        "working": {},
        "display_history": [],
        "result": {"content_type": "text/plain; charset=utf-8", "body": "ok"},
        "result_digest": None,
    }
    raw = encode_bundle_bytes(bundle)
    import hashlib

    bundle["result_digest"] = "sha256:" + hashlib.sha256(b"ok").hexdigest()
    raw = encode_bundle_bytes(bundle)
    p = tmp_path / "snapshot.json"
    p.write_bytes(raw)
    with pytest.raises(CheckpointError) as exc:
        load_snapshot_bundle(
            p,
            expected_checksum="sha256:" + hashlib.sha256(raw).hexdigest(),
            max_history_bytes=64,
            max_working_bytes=64,
        )
    assert exc.value.code == "HISTORY_LIMIT_EXCEEDED"


def test_load_snapshot_bundle_rejects_oversized_file(tmp_path) -> None:
    # 恢复路径限长: 超过 max_bundle_bytes 的文件必须拒绝(防止无界读取)。
    from ga_worker.checkpoint import encode_bundle_bytes

    bundle = {
        "schema_version": "genericagent.snapshot.v1",
        "task_id": "t1",
        "session_key": "personal:1",
        "backend_history": [],
        "agent_history": [],
        "working": {},
        "display_history": [],
        "result": {"content_type": "text/plain; charset=utf-8", "body": "ok"},
        "result_digest": "sha256:" + hashlib.sha256(b"ok").hexdigest(),
    }
    p = tmp_path / "snapshot.json"
    p.write_bytes(b"x" * 2048)
    with pytest.raises(CheckpointError) as exc:
        load_snapshot_bundle(
            p,
            expected_checksum="sha256:" + hashlib.sha256(b"x" * 2048).hexdigest(),
            max_history_bytes=4096,
            max_working_bytes=4096,
            max_bundle_bytes=1024,
        )
    assert exc.value.code == "SNAPSHOT_TOO_LARGE"


def test_load_snapshot_bundle_rejects_growth_after_stat(monkeypatch, tmp_path) -> None:
    # 审查 R5-I5: stat 之后文件被替换/持续增长的 TOCTOU——旧实现
    # path.stat() 通过后 path.read_bytes() 无界读取; 修复后必须从同一 fd
    # fstat 并按限长读取, 此场景必须报 SNAPSHOT_TOO_LARGE。
    import types

    p = tmp_path / "snapshot.json"
    p.write_bytes(b"x" * 2048)  # 实际 2048 字节, stat 谎报 32
    monkeypatch.setattr(
        Path,
        "stat",
        lambda self, *a, **k: types.SimpleNamespace(st_size=32),
    )
    with pytest.raises(CheckpointError) as exc:
        load_snapshot_bundle(
            p,
            expected_checksum="sha256:unused",
            max_history_bytes=4096,
            max_working_bytes=4096,
            max_bundle_bytes=1024,
        )
    assert exc.value.code == "SNAPSHOT_TOO_LARGE"


def test_load_snapshot_bundle_rejects_symlink(monkeypatch, tmp_path) -> None:
    # 审查 R5-I5: 最后组件符号链接必须拒绝(O_NOFOLLOW)——committed/ 位于
    # Runner 可写挂载, 攻击者可用 symlink 指向任意可读文件。
    target = tmp_path / "target.json"
    target.write_bytes(b"{}")
    link = tmp_path / "snapshot-link.json"
    try:
        os.symlink(target, link)
    except (OSError, NotImplementedError):
        pytest.skip("symlink not permitted on this platform")
    with pytest.raises(CheckpointError) as exc:
        load_snapshot_bundle(
            link,
            expected_checksum="sha256:unused",
            max_history_bytes=4096,
            max_working_bytes=4096,
            max_bundle_bytes=1024,
        )
    assert exc.value.code == "SNAPSHOT_NOT_FOUND"
