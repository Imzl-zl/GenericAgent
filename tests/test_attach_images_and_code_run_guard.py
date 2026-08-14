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
from agent_loop import media_content_blocks

# 兼容别名(2026-08-13 重构后旧名仍可用)
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

    def test_explicit_images_paths_structured(self):
        """结构化主路径: put_task(images=[...]) 显式路径 → content blocks。"""
        cwd = os.getcwd()
        try:
            tmp = self._setup()
            os.chdir(tmp)
            out = media_content_blocks("看图", ["attachments/F001_test.png"])
            assert isinstance(out, list)
            assert out[0]["type"] == "text" and out[0]["text"] == "看图"
            assert out[1]["type"] == "image_url"
            assert out[1]["image_url"]["url"].startswith("data:image/png;base64,")
        finally:
            os.chdir(cwd)

    def test_explicit_missing_file_falls_back_to_text(self):
        out = media_content_blocks("看图", ["attachments/F999_missing.png"])
        assert isinstance(out, str)  # 显式路径缺失时保留文本, 不崩

    def test_explicit_non_image_skipped(self):
        cwd = os.getcwd()
        try:
            tmp = tempfile.mkdtemp()
            os.makedirs(os.path.join(tmp, "attachments"))
            with open(os.path.join(tmp, "attachments", "F001_note.txt"), "w") as f:
                f.write("hello")
            os.chdir(tmp)
            out = media_content_blocks("读文件", ["attachments/F001_note.txt"])
            assert isinstance(out, str)  # 非图片扩展名跳过
        finally:
            os.chdir(cwd)

    def test_resize_downsample_with_pil(self):
        """PIL 可用时超长边图片被降采样(最长边 1568), 控制视觉 token 成本。"""
        try:
            from PIL import Image
        except ImportError:
            return  # 无 PIL 环境跳过
        cwd = os.getcwd()
        try:
            tmp = tempfile.mkdtemp()
            os.makedirs(os.path.join(tmp, "attachments"))
            big = Image.new("RGB", (3200, 200), (255, 0, 0))
            big.save(os.path.join(tmp, "attachments", "F001_wide.jpg"), "JPEG")
            os.chdir(tmp)
            out = media_content_blocks("看图", ["attachments/F001_wide.jpg"])
            assert isinstance(out, list)
            url = out[1]["image_url"]["url"]
            assert url.startswith("data:image/jpeg;base64,")
            # 解码回图片验证最长边 ≤ 1568
            import io
            img = Image.open(io.BytesIO(base64.b64decode(url.split(",", 1)[1])))
            assert max(img.size) <= 1568
        finally:
            os.chdir(cwd)

    def test_budget_skip_appends_placeholder(self):
        """base64 预算超限跳过的图片必须向用户显式占位(失败诚实,
        2026-08-14 审查 S1)——>2.62MiB 单图 base64 估算超 3.5MB 预算被跳过。"""
        cwd = os.getcwd()
        try:
            tmp = tempfile.mkdtemp()
            os.makedirs(os.path.join(tmp, "attachments"))
            # 3MB 图片: est = 3*4/3+64 > 3.5MB 预算 → 跳过(超单图上限内,
            # 但超 base64 总量预算, 正是审查 B3 的冲突场景)。
            with open(os.path.join(tmp, "attachments", "F001_big.jpg"), "wb") as f:
                f.write(b"\xff\xd8\xff" + b"x" * (3 * 1024 * 1024))
            os.chdir(tmp)
            out = media_content_blocks("看图", ["attachments/F001_big.jpg"])
            assert isinstance(out, str)
            assert "[图片已跳过：超出模型输入预算" in out
            assert out.startswith("看图")
        finally:
            os.chdir(cwd)


class TestBudgetMeasuredAfterDownsample:
    """2026-08-14 审查 I-2: 预算按降采样后实际注入字节判定——旧口径按
    原始文件大小估算, 大图降采样后本可注入却被误杀(est=原大小*4/3 超
    预算即跳过, 与降采样 1568px 的成本控制意图互相抵消)。"""

    def _setup_big_png(self):
        from PIL import Image
        tmp = tempfile.mkdtemp()
        os.makedirs(os.path.join(tmp, "attachments"))
        # 3000x2000 真 PNG(~4MB 原始体量), 降采样后远小于预算。
        big = Image.new("RGB", (3000, 2000), (20, 80, 200))
        path = os.path.join(tmp, "attachments", "F001_big.png")
        big.save(path, "PNG")
        return tmp, path

    def test_large_image_injected_after_downsample(self):
        try:
            from PIL import Image
        except ImportError:
            return
        cwd = os.getcwd()
        try:
            tmp, path = self._setup_big_png()
            size = os.path.getsize(path)
            # 原始文件在 3MB 单图上限内但按旧口径 est=size*4/3 会超 3.5MB
            # 预算(约 4MB PNG 必超); 新口径按降采样后实际体积, 应放行。
            assert size > 2_600_000, f"test fixture too small: {size}"
            os.chdir(tmp)
            out = media_content_blocks("看图", ["attachments/F001_big.png"])
            assert isinstance(out, list), "大图降采样后应注入而非跳过"
            images = [b for b in out if b["type"] == "image_url"]
            assert len(images) == 1
            url = images[0]["image_url"]["url"]
            import io
            img = Image.open(io.BytesIO(base64.b64decode(url.split(",", 1)[1])))
            assert max(img.size) <= 1568
        finally:
            os.chdir(cwd)

    def test_unresizable_passthrough_still_budget_limited(self):
        """PIL 解码失败原样透传的图: 预算口径自动回落为原始大小(旧语义),
        超预算仍跳过并占位——不因降采样路径而放宽安全边界。"""
        cwd = os.getcwd()
        try:
            tmp = tempfile.mkdtemp()
            os.makedirs(os.path.join(tmp, "attachments"))
            with open(os.path.join(tmp, "attachments", "F001_fake.png"), "wb") as f:
                f.write(b"\x89PNG\r\n\x1a\n" + b"x" * (3 * 1024 * 1024))  # 假 PNG 头, PIL 解码失败
            os.chdir(tmp)
            out = media_content_blocks("看图", ["attachments/F001_fake.png"])
            assert isinstance(out, str)
            assert "[图片已跳过：超出模型输入预算" in out
        finally:
            os.chdir(cwd)
