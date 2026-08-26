"""trim_messages_history 单行复合语句重构等价性回归(2026-08-26 O4)。

0c235a80 的 O(n) 优化把 4 个复合操作塞进 1 行; 重构为多行版本(成本数组 +
索引推进)后, 用单行版参考实现逐场景 diff 验证行为逐字符等价。
"""

import copy
import json
from types import SimpleNamespace

import pytest

import llmcore


def _old_trim(history, sess):
    """单行版参考实现(重构前行为基线, 逐字符等价)。"""
    cap = sess.context_win * 3
    target = int(cap * getattr(sess, 'trim_keep_rate', 0.6))
    kp = sess.trim_keep_prefix

    def cost(ms): return sum(len(json.dumps(m, ensure_ascii=False)) for m in ms)
    llmcore.compress_history_tags(history, interval=getattr(sess, 'cut_msg_interval', 5))
    llmcore.STATS.update(ctx=(c := cost(history)), msgs=len(history))
    if c <= cap: return
    llmcore.compress_history_tags(history, keep_recent=4, force=True)
    if cost(history) <= target: return
    pre, post = history[:kp], history[kp:]; costs = [len(json.dumps(m, ensure_ascii=False)) for m in post]; c = cost(pre) + sum(costs); i = 0
    while len(post) - i > 9 and c > target:
        c -= costs[i]; i += 1
        while i < len(post) and post[i].get('role') != 'user': c -= costs[i]; i += 1
        if i < len(post): old = costs[i]; post[i] = llmcore._sanitize_leading_user_msg(post[i]); costs[i] = len(json.dumps(post[i], ensure_ascii=False)); c += costs[i] - old
    post = post[i:]
    if kp and pre:
        m = pre[-1]
        if m.get('role') == 'assistant' and isinstance(m.get('content'), list):
            m['content'] = [b for b in m['content'] if not (isinstance(b, dict) and b.get('type') == 'tool_use')] or [{"type": "text", "text": "..."}]
        _d = lambda: [{"type": "text", "text": "..."}]
        gap = [{"role": "assistant", "content": _d()}] if m.get('role') == 'user' else [{"role": "user", "content": _d()}, {"role": "assistant", "content": _d()}]
        history[:] = pre + gap + post
    else: history[:] = pre + post


_WORD = "x" * 2000  # 远大于 compress 截断阈值(max_len=800), 单条成本稳定可控


def _mk_history(n, blocky=False):
    hist = []
    for i in range(n):
        role = "user" if i % 2 == 0 else "assistant"
        content = f"{_WORD} msg-{i}"
        if blocky and i % 5 == 0 and role == "assistant":
            content = [{"type": "text", "text": f"{_WORD} block-{i}"},
                       {"type": "tool_use", "id": str(i), "name": "tool", "input": {"q": _WORD}}]
        hist.append({"role": role, "content": content})
    return hist


# 场景: (名称, 消息数, blocky, sess 字段)——覆盖不触发/compress 后达标/
# 硬 trim 前中后路径(含 kp=0 与尾部非 user 的槽位更新分支)。
_CASES = [
    ("small_no_trim", 6, False, dict(context_win=20000, trim_keep_prefix=2, trim_keep_rate=0.6, cut_msg_interval=5)),
    ("compress_only", 30, False, dict(context_win=20000, trim_keep_prefix=2, trim_keep_rate=0.6, cut_msg_interval=1)),
    ("hard_trim_kp2", 40, False, dict(context_win=3000, trim_keep_prefix=2, trim_keep_rate=0.6, cut_msg_interval=5)),
    ("hard_trim_kp0", 40, False, dict(context_win=3000, trim_keep_prefix=0, trim_keep_rate=0.6, cut_msg_interval=5)),
    ("hard_trim_blocky_tail_tool", 40, True, dict(context_win=3000, trim_keep_prefix=2, trim_keep_rate=0.5, cut_msg_interval=5)),
]

@pytest.mark.parametrize("name,n,blocky,kw", _CASES, ids=[c[0] for c in _CASES])
def test_trim_refactor_equivalent_to_single_line(name, n, blocky, kw):
    sess = SimpleNamespace(**kw)
    hist_new = copy.deepcopy(_mk_history(n, blocky=blocky))
    hist_old = copy.deepcopy(_mk_history(n, blocky=blocky))

    # compress_history_tags 有模块级 _cd 计数(interval 分支共享), 每版前
    # 重置, 保证两版独立同起点。
    llmcore.compress_history_tags._cd = 0
    llmcore.trim_messages_history(hist_new, sess)
    llmcore.compress_history_tags._cd = 0
    _old_trim(hist_old, sess)

    assert hist_new == hist_old, f"case {name}: new != old\nnew={hist_new}\nold={hist_old}"