# 文档转换 SOP（Document Conversion SOP）

> 适用范围：所有文档类任务（转换/排版/合并/提取/统计）。
> 按 GA L3 规范：本文件只留**关键前置 + 典型坑**，配方在 `../assets/docx_utils.py --help`（脚本即配方）。
> 与 `wechat_delivery_sop.md` 分工：本 SOP 管"怎么做对"，交付 SOP 管"怎么交付好"。

## 0. 一句话流程

判格式 → 查 §1 前置 → pandoc 一行能转优先（用 `../assets/docx_utils.py` 或直接命令）→ pandoc 不支持的用 LibreOffice/openpyxl（§2）→ 转换后必须 verify（§3）。

## 1. 关键前置（环境事实，已实测）

- **pandoc 3.8.3 在 PATH**（镜像预装）；python-docx/openpyxl/pypdf 已装；LibreOffice 已装。
- **中文模板已预置**：`../assets/reference.docx`（黑体标题/宋体正文小四/首行缩进 2 字符/1.5 倍行距/表格 10pt，社区标准）。
- **工具脚本**：`../assets/docx_utils.py`——`python ../assets/docx_utils.py --help` 看全部用法：
  - `make-template -o 模板.docx --preset cn` 或 `--spec '{"heading1": {"font": "黑体", "size": "四号"}}'`（用户指定样式时现场造模板）
  - `md-to-docx 输入.md 输出.docx [--toc] [--number-sections] [--gfm] [--highlight]`
  - `verify 输出.docx [--expect-tables N]`
- **默认路径**：平台 `export_docx` 工具（policy 允许）已内置 md→pandoc + 字体兜底，简单转换直接用工具；样式/复杂需求按本 SOP 走脚本/命令。

## 2. 格式决策路线（实测矩阵）

| 输入 | 目标 | 正确路线 |
|------|------|----------|
| md/html/txt | docx | pandoc + 模板（`docx_utils.py md-to-docx`；精细样式 `--reference-doc`） |
| docx | md | `pandoc 输入.docx -o 输出.md` |
| docx/xlsx | **pdf** | **LibreOffice**（pandoc PDF 需 pdf-engine 未装） |
| **.doc/.xls 老格式** | docx/xlsx/csv | **LibreOffice** headless（唯一方案） |
| xlsx | 读/改/报表 | **openpyxl**（pandoc 读 xlsx 有 bug，勿试） |
| xlsx | csv | LibreOffice + FilterOptions `:44,34,76`（必须带，否则中文乱码） |
| PDF | 合并/拆分/提取 | pypdf |
| PDF | 生成 | 不支持（无 LaTeX/weasyprint）——告知用户或改交付格式 |
| md | pptx/epub | pandoc 3.8+ 直接输出 |

## 3. 典型坑（全部实测命中过）

1. **正文样式是 FirstParagraph 不是 Normal**——改模板必须动 FirstParagraph/BodyText（模板已处理，别手动只改 Normal）。
2. **中文字体必须设 eastAsia**——python-docx 只设 `run.font.name` 中文不生效（模板/脚本已处理）。
3. **pandoc 读 xlsx 必失败或乱码**（3.8.3/3.10.1 均未修）——xlsx 一律 openpyxl。
4. **LibreOffice xlsx→docx 直接转换失败**——要 docx 就 openpyxl 读 + python-docx/pandoc 生成。
5. **LibreOffice 并发/重复调用要独立 profile**——`-env:UserInstallation=file:///tmp/lo_<unique>`。
6. **pandoc 默认模板丑**——必须 `--reference-doc`（模板已预置）。
7. **GBK 编码**——Windows 来的 txt 可能 GBK；`docx_utils.py md-to-docx` 已自动容错；手动命令先 `iconv -f GBK -t UTF-8`。
8. **转换后必须验证**——不能只看退出码；`python ../assets/docx_utils.py verify 输出.docx`（重读段落/表格/标题）。
9. **输出目录**：一律 `outputs/`，文件名中文可读。
10. **Source Code 样式默认模板没有**——make-template 已自动补建（Consolas
    10pt，参照社区标准；2026-08-15 审查后语义键样式缺失不再静默丢弃）。

## 4. 样式定制（用户指定"标题黑体、一级黑体四号、二级宋体"等）

1. 默认中文模板直接可用：`python ../assets/docx_utils.py md-to-docx 输入.md outputs/输出.docx`（模板默认套用）。
2. 用户要求与默认不同 → 现场造模板：
   `python ../assets/docx_utils.py make-template -o /tmp/ref.docx --spec '{"heading1": {"font": "黑体", "size": "四号"}, "body": {"font": "宋体", "size": "小四"}}'`
   （键：heading1..6/title/body/firstparagraph/bodytext/table/compact/blocktext/sourcecode 或 styleId；属性：font/latin/size(pt 或"四号")/bold/before/after/line/indent_chars/center。`body`/`bodytext` 键会自动展开到 pandoc 正文实际使用的 FirstParagraph/BodyText，不要只改 Normal。）
3. 再 `--reference-doc=/tmp/ref.docx` 转换；或 `make-template --preset cn` 与 `--spec` 可叠加。
4. 造完按 §3.8 verify。

## 5. 自检清单（交付前）

- [ ] 输入格式走了正确路线（§2 矩阵，xlsx 没用 pandoc，PDF 没硬试）
- [ ] 样式需求按 §4 走了模板机制（不是裸 pandoc 默认）
- [ ] 产物 verify 过（段落/表格/页数）
- [ ] 文件在 outputs/，文件名可读
