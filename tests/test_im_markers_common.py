"""im_markers 收敛 + 无媒体渠道诚实降级回归测试(2026-08-14 独立审查 C1/C2)。

覆盖:
1. resolve_file_markers: 坏占位符/不存在/重复过滤, 相对路径按 base_dir 解析;
2. build_done_text 不再渲染服务器路径("生成文件:" 文本交付已移除);
3. AgentChatMixin.send_done: 有文件产出但 can_send_media=False 时输出
   诚实提示而非路径。
"""

import sys
import types
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_ROOT / "frontends"))
sys.path.insert(0, str(_ROOT))


@pytest.fixture(autouse=True)
def _stub_agentmain():
    """chatapp_common 底部 import agentmain; 测试只验证 marker 语义,
    用最小桩避免拉起真实 agent 依赖。恢复时还原既有模块(2026-08-14
    复审: 原实现无条件 pop, 若同进程其他测试先导入真实 agentmain 会被
    驱逐后重载, 副作用重复)。"""
    mods = {}
    saved = {}
    for name in ("agentmain", "continue_cmd", "btw_cmd", "review_cmd"):
        saved[name] = sys.modules.get(name)
        m = types.ModuleType(name)
        if name == "agentmain":
            class _GA:
                pass
            m.GeneraticAgent = _GA
        else:
            m.handle_frontend_command = lambda *a, **k: None
            m.install = lambda *a, **k: None
            m.handle = lambda *a, **k: None
            m.handle_command = lambda *a, **k: None
            m.reset_conversation = lambda *a, **k: None
        mods[name] = m
    for name, m in mods.items():
        sys.modules[name] = m
    yield
    for name in mods:
        if saved[name] is not None:
            sys.modules[name] = saved[name]
        else:
            sys.modules.pop(name, None)


def _reload_chatapp_common():
    import importlib
    import chatapp_common as cc
    return importlib.reload(cc)


def test_resolve_file_markers_filters_and_dedups(tmp_path):
    from im_markers import resolve_file_markers

    (tmp_path / "a.png").write_bytes(b"x")
    (tmp_path / "sub").mkdir()
    (tmp_path / "sub" / "b.docx").write_bytes(b"y")
    text = (
        "[FILE:a.png] [FILE:missing.png] [FILE:<path>] [FILE:sub/b.docx] "
        "[FILE:sub/b.docx] [FILE:...]"
    )
    got = resolve_file_markers(text, base_dir=str(tmp_path))
    assert got == [str(tmp_path / "a.png"), str(tmp_path / "sub" / "b.docx")], got


def test_build_done_text_never_renders_server_path(tmp_path):
    cc = _reload_chatapp_common()
    f = tmp_path / "x.png"
    f.write_bytes(b"x")
    text = f"任务完成\n[FILE:{f}]"
    out = cc.build_done_text(text)
    assert "生成文件" not in out
    assert "x.png" not in out
    assert out == "任务完成"


def test_send_done_honest_fallback_without_media_capability(tmp_path):
    cc = _reload_chatapp_common()
    f = tmp_path / "x.png"
    f.write_bytes(b"x")

    sent = []

    class Mixin(cc.AgentChatMixin):
        label, source = "Test", "test"

        async def send_text(self, chat_id, content, **ctx):
            sent.append(content)

    m = Mixin(None, {})
    assert m.can_send_media is False

    import asyncio
    asyncio.run(m.send_done("c1", f"结果如下\n[FILE:{f}]"))
    assert len(sent) == 1
    assert "不支持直接发送" in sent[0]
    assert str(f) not in sent[0]

    # 无文件时走纯文本(不出现提示)。
    sent.clear()
    asyncio.run(m.send_done("c1", "只有文本"))
    assert sent == ["只有文本"]
