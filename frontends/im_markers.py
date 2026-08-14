"""IM 前端 [FILE:] marker 纯函数(2026-08-14 独立审查 C2 收敛)。

根前端 wechatapp/tgapp/fsapp/wecomapp 各自实现了 marker 提取+解析, 语义
微差且重复; 收敛到本模块(无副作用, 不 import agentmain——chatapp_common
底部有模块级安装副作用, 独立纯模块避免引入)。发送仍由各渠道自实现。

设计: 提取(extract_files) → 解析(resolve_file_markers) 两段式; 解析只做
"坏占位符过滤 + 路径解析 + 存在性 + 去重", 渠道特有的过滤(如 wechatapp
排除用户刚发送的 media_paths)留在渠道侧。
"""
import os
import re

#: 模型常见的伪 marker 占位符(视为"不知道具体路径", 不解析)。
BAD_FILE_MARKERS = {'filepath', '<filepath>', 'path', '<path>',
                    'file_path', '<file_path>', '...'}


def extract_files(text):
    return re.findall(r"\[FILE:([^\]]+)\]", text or "")


def strip_files(text):
    return re.sub(r"\[FILE:[^\]]+\]", "", text or "").strip()


def resolve_file_markers(text, base_dir=None, bad=None):
    """提取 [FILE:...] marker 并解析为存在且去重的路径列表。

    base_dir: 相对 marker 的解析根(根前端统一 temp/); None = 保持原样
    (相对路径按进程 cwd 判存在性, fsapp 语义)。
    bad: 额外坏占位符集合(默认 BAD_FILE_MARKERS)。
    """
    bad = bad or BAD_FILE_MARKERS
    out, seen = [], set()
    for p in extract_files(text or ''):
        key = (p or '').strip().lower()
        if key in bad:
            continue
        fp = p if os.path.isabs(p) else (os.path.join(base_dir, p) if base_dir else p)
        fp = os.path.normpath(fp)
        if fp in seen or not os.path.isfile(fp):
            continue
        seen.add(fp)
        out.append(fp)
    return out
