"""tgapp 轮次协调终态回归测试(2026-08-12 审查修复)。

输出分层后流内无轮次标记, 纯工具收尾轮(最后会话无正文)时, done 的全量
累计文本不得注入空会话——此前各轮文本已随 turn 事件各自定型, 再发 =
全量重复。终态应为完成确认, 且附件不丢。
"""

import sys
import types
from types import SimpleNamespace

import pytest
from pathlib import Path

# --- stub telegram(测试环境不装 python-telegram-bot) ---
def _stub_telegram():
    mods = {}
    t = types.ModuleType("telegram")
    t.BotCommand = object
    t.InlineKeyboardButton = object
    t.InlineKeyboardMarkup = object
    mods["telegram"] = t
    const = types.ModuleType("telegram.constants")
    const.ChatType = SimpleNamespace(PRIVATE="private", GROUP="group", SUPERGROUP="supergroup")
    const.MessageLimit = SimpleNamespace(MAX_TEXT_LENGTH=4096)
    const.ParseMode = SimpleNamespace(MARKDOWN_V2="MarkdownV2", HTML="HTML")
    mods["telegram.constants"] = const
    err = types.ModuleType("telegram.error")
    err.RetryAfter = Exception
    mods["telegram.error"] = err
    ext = types.ModuleType("telegram.ext")
    ext.ApplicationBuilder = object
    ext.CallbackQueryHandler = object
    ext.MessageHandler = object
    ext.ContextTypes = SimpleNamespace(DEFAULT_TYPE=object)
    mods["telegram.ext"] = ext
    filt = types.ModuleType("telegram.ext.filters")
    filt.TEXT = object
    ext.filters = filt
    helpers = types.ModuleType("telegram.helpers")
    helpers.escape_markdown = lambda s, *a, **k: s
    mods["telegram.helpers"] = helpers
    req = types.ModuleType("telegram.request")
    req.HTTPXRequest = object
    mods["telegram.request"] = req
    for name, m in mods.items():
        sys.modules[name] = m


_stub_telegram()

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "frontends"))
import tgapp  # noqa: E402


class _FakeSession:
    """记录 add_chunk/finalize 调用的最小会话替身。"""

    def __init__(self, root_msg):
        self.root_msg = root_msg
        self.raw_text = ""
        self.finalized_texts = []

    async def prime(self):
        pass

    async def add_chunk(self, chunk):
        self.raw_text += chunk

    async def finalize(self, full_text=None, send_files=True):
        if full_text is not None:
            self.raw_text = full_text
        self.finalized_texts.append((self.raw_text, send_files))


class _RootMsg:
    pass


@pytest.fixture(autouse=True)
def _patch_tgapp(monkeypatch):
    monkeypatch.setattr(tgapp, "_TelegramStreamSession", _FakeSession)


async def _send_files_from_text(root, text):
    _send_files_from_text.calls.append(text)


def test_finalize_empty_last_turn_no_duplicate(monkeypatch):
    monkeypatch.setattr(tgapp, "_send_files_from_text", _send_files_from_text)
    _send_files_from_text.calls = []

    async def scenario():
        coord = tgapp._TelegramTurnStreamCoordinator(_RootMsg())
        await coord.prime()
        session1 = coord.session
        # 第一轮: 文本增量到达。
        await coord.add_chunk("第一轮回复\n")
        # 第二轮边界事件: 上一轮有正文 → 定型并开新会话(草稿)。
        await coord.on_turn()
        assert session1.finalized_texts and session1.finalized_texts[-1][0] == "第一轮回复\n"
        session2 = coord.session
        # 第二轮是纯工具轮: 无任何文本, 直接终态。
        await coord.finalize(done_text="第一轮回复\n")
        # 空会话不得注入全量累计文本(防重复), 以完成确认收尾。
        assert session2.finalized_texts[-1][0] == "✅ 已完成"
        # 附件(全量文本中的 [FILE:...])不丢。
        assert _send_files_from_text.calls == ["第一轮回复\n"]

    _run(scenario)


def test_finalize_last_turn_has_text_keeps_own_text(monkeypatch):
    monkeypatch.setattr(tgapp, "_send_files_from_text", _send_files_from_text)
    _send_files_from_text.calls = []

    async def scenario():
        coord = tgapp._TelegramTurnStreamCoordinator(_RootMsg())
        await coord.prime()
        session1 = coord.session
        await coord.add_chunk("第一轮回复\n")
        await coord.on_turn()
        # 最后一轮有正文: 终态用会话自身文本, 不注入 done_text。
        await coord.add_chunk("第二轮回复\n")
        await coord.finalize(done_text="第一轮回复\n第二轮回复\n")
        assert coord.session.finalized_texts[-1][0] == "第二轮回复\n"
        assert _send_files_from_text.calls == ["第一轮回复\n第二轮回复\n"]

    _run(scenario)


def _run(coro):
    import asyncio
    asyncio.new_event_loop().run_until_complete(coro())
