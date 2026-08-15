"""L1 动态 SOP 披露测试(2026-08-15)。

背景: 静态 L1 索引(global_mem_insight.txt ← assets/global_mem_insight_template.txt)
漏录平台新增 SOP(document_conversion_sop/wechat_delivery_sop)导致模型从未读到
它们(生产 file_access_stats 实证)。修复: get_global_memory() 扫描 memory/ 顶层
自动披露未索引的 *.md/*.py——渐进披露不再依赖索引人工维护。
"""
import os

import ga


def _make_ga_env(tmp_path, monkeypatch):
    """构造最小 GA 目录(assets/ + memory/)并指向 ga.script_dir。

    显式固定 GA_LANG=zh(其他测试 import agentmain 时 setdefault 可能按
    locale 置 en, 进程级污染本测试的中文模板路径)。
    """
    monkeypatch.setenv("GA_LANG", "zh")
    assets = tmp_path / "assets"
    memory = tmp_path / "memory"
    assets.mkdir()
    memory.mkdir()
    (assets / "insight_fixed_structure.txt").write_text(
        "Facts(L2): ../memory/global_mem.txt | SOPs(L3): ../memory/*.md or *.py\n",
        encoding="utf-8",
    )
    (memory / "global_mem_insight.txt").write_text(
        "# [Global Memory Insight]\nL3: memory_cleanup_sop(memory cleanup)\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(ga, "script_dir", str(tmp_path))
    return memory


def test_auto_discovered_sops_lists_unindexed_md(tmp_path, monkeypatch):
    memory = _make_ga_env(tmp_path, monkeypatch)
    # 未索引的新 SOP + 已索引的(不应重复列出) + 非 md/py(不应列出)。
    (memory / "document_conversion_sop.md").write_text("# sop", encoding="utf-8")
    (memory / "memory_cleanup_sop.md").write_text("# indexed", encoding="utf-8")
    (memory / "README.txt").write_text("not a sop", encoding="utf-8")
    (memory / "sub").mkdir()
    (memory / "sub" / "nested.md").write_text("x", encoding="utf-8")

    out = ga.get_global_memory()
    assert "document_conversion_sop.md" in out, "unindexed SOP must be auto-disclosed"
    assert "memory_cleanup_sop.md" not in out, "indexed SOP must not be duplicated"
    assert "README.txt" not in out, "non md/py must be excluded"
    assert "[L3 auto-discovered SOPs/utils]" in out


def test_auto_discovered_sops_empty_when_all_indexed(tmp_path, monkeypatch):
    memory = _make_ga_env(tmp_path, monkeypatch)
    (memory / "memory_cleanup_sop.md").write_text("# indexed", encoding="utf-8")
    (memory / "ocr_utils.py").write_text("x = 1", encoding="utf-8")
    # 静态索引含 base 名即视为已索引(含注释语义), 不再重复。
    (memory / "global_mem_insight.txt").write_text(
        "# [Global Memory Insight]\nL3: memory_cleanup_sop | ocr_utils.py\n",
        encoding="utf-8",
    )
    out = ga.get_global_memory()
    assert "auto-discovered" not in out


def test_auto_discovered_sops_substring_collision_not_suppressed(tmp_path, monkeypatch):
    """审查 B-6: 去重必须按索引词精确匹配——"cleanup_sop" 是
    "memory_cleanup_sop" 的子串, 子串匹配会漏披露新 SOP(恰是本轮要防的
    事故类别: 新增 SOP 不被模型读到)。"""
    memory = _make_ga_env(tmp_path, monkeypatch)
    # 索引里有 memory_cleanup_sop, 新 SOP cleanup_sop 名字是其子串。
    (memory / "cleanup_sop.md").write_text("# sop", encoding="utf-8")
    out = ga.get_global_memory()
    assert "cleanup_sop.md" in out, "substring-colliding SOP must still be disclosed"


def test_auto_discovered_sops_tolerates_missing_memory(tmp_path, monkeypatch):
    _make_ga_env(tmp_path, monkeypatch)
    # memory/ 不存在: 不抛异常, 无 auto 段。
    import shutil
    shutil.rmtree(tmp_path / "memory")
    out = ga.get_global_memory()
    assert "auto-discovered" not in out
