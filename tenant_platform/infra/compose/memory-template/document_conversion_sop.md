# 文档转换 SOP（Document Conversion SOP）

> 适用范围：所有文档类任务（转换/排版/合并/提取/统计）。
> 按 GA L3 规范：本文件只留**关键前置 + 典型坑**，配方在 `../assets/docx_utils.py --help`（脚本即配方）。
> 与 `wechat_delivery_sop.md` 分工：本 SOP 管"怎么做对"，交付 SOP 管"怎么交付好"。

## 0. 一句话流程

判格式 → 查 §1 前置 → **默认走平台 `export_docx` 工具**（内置模板 + 验证）→ 样式要定制才用 `docx_utils.py make-template` + `md-to-docx`（§4）→ pandoc 不支持的用 LibreOffice/openpyxl（§2）→ 自写路径必须 verify（§3.8）。

## 1. 关键前置（环境事实，已实测）

- **pandoc 3.8.3 在 PATH**（镜像预装）；python-docx/openpyxl/pypdf 已装；LibreOffice 已装。
- **中文模板已预置**：`assets/reference.docx`（**宋体正文小四/首行缩进 2 字符/1.5 倍行距/黑体标题/西文 Times New Roman/表格全框线+浅蓝表头**，社区标准，2026-08-15 模板化升级）。
- **默认路径 = `export_docx` 工具**（policy 允许）：md/html→pandoc + 内置模板，`title` 参数自动生成封面标题（pandoc `--metadata=title`），**转换后自动验证**（段落/表格/封面断言，失败删除产物并显式报错）。纯文本自动走 python-docx（仅设中文 eastAsia=宋体，西文保持默认）。
- **工具脚本**：`../assets/docx_utils.py`——`python ../assets/docx_utils.py --help` 看全部用法：
  - `make-template -o 模板.docx --preset cn` 或 `--spec '{"heading1": {"font": "黑体", "size": "四号"}}'`（用户指定样式时现场造模板）
  - `md-to-docx 输入.md 输出.docx [--toc] [--number-sections] [--gfm] [--title "封面标题"]`
  - `verify 输出.docx [--expect-tables N] [--expect-title "封面标题"]`

## 2. 格式决策路线（实测矩阵）

| 输入 | 目标 | 正确路线 |
|------|------|----------|
| md/html/txt | docx | **`export_docx`（默认，模板内置）**；样式定制才用 `md-to-docx` + 模板 |
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
2. **中文字体必须设 eastAsia**——python-docx 只设 `run.font.name` 中文不生效（模板/脚本已处理）。**西文不要设成中文字体**（全雅黑会让数字/字母宽重，非 Windows 平台还会回退）。
3. **pandoc 读 xlsx 必失败或乱码**（3.8.3/3.10.1 均未修）——xlsx 一律 openpyxl。
4. **LibreOffice xlsx→docx 直接转换失败**——要 docx 就 openpyxl 读 + python-docx/pandoc 生成。
5. **LibreOffice 并发/重复调用要独立 profile**——`-env:UserInstallation=file:///tmp/lo_<unique>`。
6. **pandoc 默认模板丑**——必须 `--reference-doc`（export_docx 已内置模板；手动 pandoc 必须带模板，裸调=白做）。
7. **GBK 编码**——Windows 来的 txt 可能 GBK；`export_docx`/`md-to-docx` 已自动容错；手动命令先 `iconv -f GBK -t UTF-8`。
8. **转换后必须验证**——不能只看退出码；`export_docx` 已内置（失败自动删产物报错），自写路径必须 `python ../assets/docx_utils.py verify 输出.docx`。
9. **输出目录**：一律 `outputs/`，文件名中文可读。
10. **Source Code 样式默认模板没有**——make-template 已自动补建（Consolas 10pt，参照社区标准；语义键样式缺失不再静默丢弃）。
11. **不要自写 python-docx 脚本做标准 md→docx 转换**——默认模板 + export_docx 已覆盖"排版格式搞好"（重复造轮子 = 今天旧事故根源）。仅当**模板机制覆盖不了**（复杂报表/多表头/条件着色等精细排版）才手写 python-docx，且必须 verify。

## 4. 样式定制（用户指定"标题黑体、一级黑体四号、二级宋体、要目录"等）

1. 默认中文模板直接可用：`export_docx`（模板内置）；`md-to-docx 输入.md outputs/输出.docx`（模板默认套用）。
2. 常用变体一句话：
   - 要目录：`md-to-docx 输入.md outputs/输出.docx --toc`
   - 要标题编号：`md-to-docx 输入.md outputs/输出.docx --number-sections`
   - 要封面标题：`export_docx` 传 `title`；或 `md-to-docx --title "封面标题"`
   - 用户要求与默认不同（字体/字号）→ 现场造模板：
   `python ../assets/docx_utils.py make-template -o /tmp/ref.docx --spec '{"heading1": {"font": "黑体", "size": "四号"}, "body": {"font": "宋体", "size": "小四"}}'`
   （键：heading1..6/title/body/firstparagraph/bodytext/table/compact/blocktext/sourcecode 或 styleId；属性：font/latin/size(pt 或"四号")/bold/before/after/line/indent_chars/center/header_fill。`body`/`bodytext` 键自动展开到 pandoc 正文实际使用的 FirstParagraph/BodyText，不要只改 Normal。）
3. 再 `md-to-docx --template /tmp/ref.docx` 转换；`--preset cn` 与 `--spec`
   可组合使用——注意**按样式粒度覆盖**（--spec 的键整体替换该样式的预设
   条目，不是属性级合并；要保留预设字号/行距时把需要的属性一起写进 spec）。
4. **个别段落/块要不同样式**（如某段不缩进、某块用 Source Code）：pandoc
   官方 custom-style 机制（fenced div）——`::: {custom-style="样式名"}` 包裹，
   样式名须已存在于所用模板（make-template 已覆盖模板内置样式）；
   `md-to-docx` 直通 pandoc 无需额外参数。
5. 造完按 §3.8 verify。

## 5. 自检清单（交付前）

- [ ] 输入格式走了正确路线（§2 矩阵，xlsx 没用 pandoc，PDF 没硬试）
- [ ] 简单 md→docx 用了 export_docx（没手写 python-docx 脚本）
- [ ] 样式需求按 §4 走了模板机制（不是裸 pandoc 默认）
- [ ] 产物验证过（export_docx 内置或手动 verify：段落/表格/封面标题）
- [ ] 文件在 outputs/，文件名可读
