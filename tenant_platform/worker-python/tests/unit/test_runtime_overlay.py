"""Unit tests for runtime overlay reuse integrity (content digest validation)."""

from __future__ import annotations

import pytest

from ga_worker.runtime_overlay import OVERLAY_MANIFEST_ENTRIES, materialize_runtime_overlay


@pytest.fixture
def roots(tmp_path):
    legacy_root = tmp_path / "legacy"
    runtime_root = tmp_path / "runtime"
    legacy_root.mkdir()
    runtime_root.mkdir()
    # Minimal legacy tree: one module + one plugin + one asset.
    (legacy_root / "agentmain.py").write_text("# agentmain v1\n", encoding="utf-8")
    (legacy_root / "ga.py").write_text("# ga v1\n", encoding="utf-8")
    (legacy_root / "llmcore.py").write_text("# llmcore v1\n", encoding="utf-8")
    (legacy_root / "agent_loop.py").write_text("# agent_loop v1\n", encoding="utf-8")
    (legacy_root / "simphtml.py").write_text("# simphtml v1\n", encoding="utf-8")
    plugins = legacy_root / "plugins"
    plugins.mkdir()
    (plugins / "__init__.py").write_text("", encoding="utf-8")
    (plugins / "hooks.py").write_text("# hooks v1\n", encoding="utf-8")
    (plugins / "project_mode.py").write_text("# project_mode v1\n", encoding="utf-8")
    assets = legacy_root / "assets"
    assets.mkdir()
    for name in (
        "tools_schema.json",
        "tools_schema_cn.json",
        "sys_prompt.txt",
        "sys_prompt_en.txt",
        "global_mem_insight_template.txt",
        "global_mem_insight_template_en.txt",
        "insight_fixed_structure.txt",
        "insight_fixed_structure_en.txt",
        "code_run_header.py",
    ):
        (assets / name).write_text(f"# {name}\n", encoding="utf-8")
    return legacy_root, runtime_root


def test_overlay_reuse_when_sources_unchanged(roots):
    legacy_root, runtime_root = roots
    overlay_dir, manifest = materialize_runtime_overlay(
        legacy_root=legacy_root, runtime_root=runtime_root, session_id="s-abc"
    )
    again_dir, again_manifest = materialize_runtime_overlay(
        legacy_root=legacy_root, runtime_root=runtime_root, session_id="s-abc"
    )
    assert again_dir == overlay_dir
    assert again_manifest == manifest
    assert (overlay_dir / "OVERLAY_MANIFEST.json").is_file()


def test_overlay_rebuilt_when_legacy_source_changes(roots):
    legacy_root, runtime_root = roots
    overlay_dir, _ = materialize_runtime_overlay(
        legacy_root=legacy_root, runtime_root=runtime_root, session_id="s-abc"
    )
    # 模拟镜像升级: legacy 源内容变化。
    (legacy_root / "ga.py").write_text("# ga v2 upgraded\n", encoding="utf-8")
    overlay_dir2, manifest2 = materialize_runtime_overlay(
        legacy_root=legacy_root, runtime_root=runtime_root, session_id="s-abc"
    )
    assert overlay_dir2 == overlay_dir  # 同一路径, 但内容已重建
    assert (overlay_dir / "ga.py").read_text(encoding="utf-8") == "# ga v2 upgraded\n"
    # manifest 的 digest 已更新。
    assert manifest2["sha256"]["ga.py"]


def test_overlay_rebuilt_when_overlay_file_tampered(roots):
    """Runner 篡改 overlay 副本后, 下次 StartSession 必须重建而不是保留篡改内容。"""
    legacy_root, runtime_root = roots
    overlay_dir, _ = materialize_runtime_overlay(
        legacy_root=legacy_root, runtime_root=runtime_root, session_id="s-abc"
    )
    ga_file = overlay_dir / "ga.py"
    ga_file.chmod(ga_file.stat().st_mode | 0o200)  # 解除只读后才能模拟篡改
    ga_file.write_text("# tampered by runner\n", encoding="utf-8")
    # 重建后恢复为 legacy 源内容。
    materialize_runtime_overlay(
        legacy_root=legacy_root, runtime_root=runtime_root, session_id="s-abc"
    )
    assert ga_file.read_text(encoding="utf-8") == "# ga v1\n"


def test_overlay_materializes_under_overlay_root(roots, tmp_path):
    """审查 R4-I13: overlay_root 指定时(容器 tmpfs), overlay 必须落在
    overlay_root 下, runtime_root 不产生 overlay 残留。"""
    legacy_root, runtime_root = roots
    overlay_root = tmp_path / "overlay"
    overlay_root.mkdir()
    overlay_dir, manifest = materialize_runtime_overlay(
        legacy_root=legacy_root,
        runtime_root=runtime_root,
        session_id="s-xyz",
        overlay_root=overlay_root,
    )
    assert str(overlay_dir).startswith(str(overlay_root))
    assert not (runtime_root / "s-xyz").exists(), "overlay must not persist under runtime_root"
    assert manifest["session_id"] == "s-xyz"
    # 再次物化应复用(校验通过), 路径不变。
    again_dir, _ = materialize_runtime_overlay(
        legacy_root=legacy_root,
        runtime_root=runtime_root,
        session_id="s-xyz",
        overlay_root=overlay_root,
    )
    assert again_dir == overlay_dir
