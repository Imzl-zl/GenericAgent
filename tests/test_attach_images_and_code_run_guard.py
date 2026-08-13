"""2026-08-13 生产实证回归: 图片任务 300s 超时根因修复。

根因: ①ga.py code_run 把模型 script 当纯 Python 写入 .py 执行, 而
agnes-2.5-flash 等模型生成 shell 风格脚本(python3 -c/pip install/&&),
SyntaxError 后 agent 无限重试直到任务超时(生产 307s task timeout,
0 chunk 零回复)。②GA 只把图片路径文本给模型, 多模态模型收不到图片
内容, 只能靠 code_run 瞎折腾。

修复: ①code_run python 模式检测 shell 特征自动降级 bash;
②agent_loop 首轮 user content 注入附件图片 image_url 块(base64 直传)。
"""

import base64
import os
import tempfile

from ga import _SHELL_STYLE_RE
from agent_loop import _inject_attachment_images

_1PX_PNG = bytes.fromhex(
    "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c489"
    "0000000d4944415478da63fcffff3f0300050001ff1aa1e66e0000000049454e44ae426082"
)


class TestShellStyleDetection:
    def test_shell_style_hits(self):
        for code in [
            'python3 -c "from PIL import Image"',
            'pip install Pillow pytesseract -q',
            "cd /tmp && ls -la",
            "sudo apt-get update",
            "curl -s http://x",
            "#!/bin/bash\necho hi",
        ]:
            assert _SHELL_STYLE_RE.search(code), f"should detect shell: {code!r}"

    def test_pure_python_misses(self):
        for code in [
            "import datetime\nprint(datetime.datetime.now())",
            'from PIL import Image\nimg = Image.open("attachments/x.jpg")',
            "echo(1)\nls('x')",  # Python 调用, 后跟 '(' 不命中
            "print(a | b)",  # 位或不是管道
            "x = [1, 2]\nprint(x)",
        ]:
            assert not _SHELL_STYLE_RE.search(code), f"should keep python: {code!r}"


class TestAttachmentImageInjection:
    def _setup(self):
        tmp = tempfile.mkdtemp()
        os.makedirs(os.path.join(tmp, "attachments"))
        with open(os.path.join(tmp, "attachments", "F001_test.png"), "wb") as f:
            f.write(_1PX_PNG)
        return tmp

    def test_inject_image_block(self):
        cwd = os.getcwd()
        try:
            tmp = self._setup()
            os.chdir(tmp)
            out = _inject_attachment_images(
                "这是啥 [Session file workspace] attachments/F001_test.png => attachments/F001_test.png"
            )
            assert isinstance(out, list)
            assert out[0] == {"type": "text", "text": "这是啥 [Session file workspace] attachments/F001_test.png => attachments/F001_test.png"}
            assert out[1]["type"] == "image_url"
            url = out[1]["image_url"]["url"]
            assert url.startswith("data:image/png;base64,")
            assert base64.b64decode(url.split(",", 1)[1]) == _1PX_PNG
        finally:
            os.chdir(cwd)

    def test_no_attachment_keeps_string(self):
        assert _inject_attachment_images("纯文本") == "纯文本"

    def test_missing_file_keeps_string(self):
        cwd = os.getcwd()
        try:
            tmp = tempfile.mkdtemp()  # 无 attachments 目录
            os.chdir(tmp)
            out = _inject_attachment_images("看图 attachments/F999_missing.jpg")
            assert isinstance(out, str)
        finally:
            os.chdir(cwd)

    def test_max_count_and_dup_dedup(self):
        cwd = os.getcwd()
        try:
            tmp = self._setup()
            for i in range(2, 6):
                with open(os.path.join(tmp, "attachments", f"F00{i}_t.png"), "wb") as f:
                    f.write(_1PX_PNG)
            os.chdir(tmp)
            text = " ".join(f"attachments/F00{i}_t.png" for i in range(1, 6))
            text += " attachments/F001_test.png"  # 重复引用去重
            out = _inject_attachment_images(text)
            assert isinstance(out, list)
            images = [b for b in out if b["type"] == "image_url"]
            assert len(images) == 3  # 上限 3 张
        finally:
            os.chdir(cwd)
