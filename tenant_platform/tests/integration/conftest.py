"""平台 E2E 共享 fixture。

assets/reference.docx 是 ga-runner 构建期产物(docx_utils.py make-template
生成, 不入库), worker 物化 runtime overlay 的 LEGACY_ASSETS 清单要求该
文件存在(缺失 = OVERLAY_ERROR, 生产 fail-closed 正确)。E2E 以仓库根为
legacy root, 缺失时生成最小占位 docx(物化只复制不校验内容, E2E 不测
转换功能), 结束后清理。与 worker test_worker_rpc.py 的
reference_docx_placeholder 模式一致。
"""
from __future__ import annotations

from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
REFERENCE_DOCX = REPO_ROOT / "assets" / "reference.docx"


@pytest.fixture(scope="session", autouse=True)
def reference_docx_placeholder():
    generated = False
    if not REFERENCE_DOCX.is_file():
        try:
            from docx import Document
        except ImportError:
            # 环境无 python-docx: 不静默, 让后续 overlay 物化显式失败。
            yield
            return
        Document().save(str(REFERENCE_DOCX))
        generated = True
        print("[itest] generated placeholder assets/reference.docx")
    yield
    if generated:
        REFERENCE_DOCX.unlink(missing_ok=True)
