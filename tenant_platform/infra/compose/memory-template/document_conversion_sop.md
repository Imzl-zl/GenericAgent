# 文档转换 SOP（Document Conversion SOP）

> 适用范围：所有文档类任务（转换/排版/合并/提取/统计）。
> 工具链为 runner 镜像内置能力，**本地直接执行**，不走任何 MCP。
> 与 `wechat_delivery_sop.md` 分工：本 SOP 管"怎么做对"，交付 SOP 管"怎么交付好"。
> 本 SOP 的能力矩阵均经**实测验证**（2026-08-08），不是软件官方宣传。

## 1. 工具矩阵（镜像内置，实测版本与能力）

| 工具 | 版本 | 可靠场景（实测） | 不可用场景（实测踩坑） |
|------|------|------------------|------------------------|
| pandoc | 3.8.3（官方二进制） | md/html/docx/odt/epub/rst/org/latex/ipynb/csv **互转**，中文正常 | **xlsx 输入**：官方 3.8.3 与最新 3.10.1 均有 bug（openpyxl 生成的 rels 绝对路径解析成 `xl//xl/...` 直接失败；LibreOffice 生成的虽能读但中文乱码）。**PDF 输出**：需 pdf-engine，默认 pdflatex 未装 |
| LibreOffice | 7.4.7（headless） | **docx/xlsx → pdf 高保真渲染**（中文完美）；**.doc/.xls 老格式**读转换；xlsx→csv | **xlsx→docx 直接转换失败**（Calc→Writer 过滤不可靠），不要用 |
| python-docx | 1.2.0 | Word .docx 读写、精细排版（封面/主题色/页眉页脚/表格样式） | .doc 老格式 |
| openpyxl | 3.1.5 | Excel .xlsx **读写唯一可靠路径**（单元格/公式/样式/多 sheet） | .xls 老格式 |
| pypdf | 6.15.0 | PDF 读取/提取/拆分/合并 | 生成 PDF |

**选择顺序**：判格式 → 查本表可靠场景 → pandoc 一行命令能转优先 → 精细控制用 Python 库 → 老格式/PDF 生成用 LibreOffice。

## 2. 格式决策路线（按输入格式）

| 输入 | 目标 | 正确路线 |
|------|------|----------|
| md/html/txt | docx | `pandoc in.md -o out.docx`（精细样式加 `--reference-doc`） |
| docx | md | `pandoc in.docx -o out.md` |
| docx | **pdf** | `soffice --headless --convert-to pdf in.docx --outdir outputs/`（高保真） |
| xlsx | 读取/分析/修改 | **openpyxl**（`load_workbook`），不要用 pandoc |
| xlsx | csv | LibreOffice：`--convert-to 'csv:Text - txt - csv (StarCalc):44,34,76'`（44=`,` 34=`"` 76=UTF-8，**必须指定否则中文乱码**） |
| xlsx | docx（报表/文档） | openpyxl 读数据 → python-docx/pandoc 生成（数据流拼接） |
| **.doc / .xls**（老格式） | docx/xlsx/csv | LibreOffice headless 转换（唯一方案） |
| PDF | 合并/拆分/提取 | pypdf |
| PDF | 生成 | **不支持**（无 LaTeX/weasyprint）——告知用户或改交付格式 |

## 3. 常用配方

```bash
# pandoc 文本类转换（xlsx 除外）
pandoc 输入.md -o outputs/输出.docx
pandoc 输入.md -o outputs/输出.docx --toc --toc-depth=2
pandoc -o /tmp/ref.docx --print-default-data-file reference.docx   # 样式模板
pandoc 输入.md -o outputs/输出.docx --reference-doc=/tmp/ref.docx
pandoc 输入.docx -o outputs/输出.md

# LibreOffice（headless，必须带 -env:UserInstallation 防 profile 冲突）
soffice --headless --norestore -env:UserInstallation=file:///tmp/lo_profile \
  --convert-to pdf 输入.docx --outdir outputs/
soffice --headless --norestore -env:UserInstallation=file:///tmp/lo_profile \
  --convert-to 'csv:Text - txt - csv (StarCalc):44,34,76' 输入.xlsx --outdir outputs/
soffice --headless --norestore -env:UserInstallation=file:///tmp/lo_profile \
  --convert-to docx 输入.doc --outdir outputs/     # 老格式
```

```python
# Excel（openpyxl 唯一可靠路径）
from openpyxl import load_workbook
wb = load_workbook('素材.xlsx')           # 读
ws = wb.active
for row in ws.iter_rows(values_only=True): print(row)
ws['A1'] = 值; wb.save('outputs/结果.xlsx')  # 写

# PDF（pypdf）
from pypdf import PdfReader, PdfWriter
w = PdfWriter()
for f in ['a.pdf', 'b.pdf']:
    for p in PdfReader(f).pages: w.add_page(p)
w.write('outputs/合并.pdf')

# Word 精细排版（python-docx）
from docx import Document
from docx.shared import Pt, RGBColor
d = Document()
h = d.add_heading('封面标题', level=0)     # level 0 = 封面大标题
p = d.add_paragraph('正文')
run = p.runs[0]; run.font.size = Pt(12); run.font.name = '微软雅黑'
run.font.color.rgb = RGBColor(0x1F, 0x3B, 0x73)
d.add_page_break(); d.add_heading('第一章', level=1)
d.save('outputs/结果.docx')
```

## 4. 常见坑（全部实测命中过）

1. **pandoc 读 xlsx 必失败或乱码**（官方 3.8.3/3.10.1 均未修）——xlsx 一律 openpyxl/LibreOffice，别浪费时间试 pandoc。
2. **LibreOffice xlsx→docx 直接转换失败**（写入错误 0xc10）——要 docx 就 openpyxl 读数据 + python-docx 生成。
3. **LibreOffice csv 导出默认编码乱码**——必须带 FilterOptions `:44,34,76`（UTF-8）。
4. **LibreOffice 并发/重复调用要独立 profile**——`-env:UserInstallation=file:///tmp/lo_<unique>`，共用 profile 会锁冲突。
5. **中文排版**：pandoc 默认 docx 样式中文可读但朴素；要"好看"用 reference-doc 模板或 python-docx 显式设中文字体。
6. **图片资源**：md 转 docx 时相对路径图片须在转换前存在，缺失会警告跳过。
7. **编码**：从 Windows 来的 txt 可能是 GBK，先 `iconv -f GBK -t UTF-8` 再处理。
8. **转换后必须验证**：不能只看命令退出码——docx 用 python-docx 重读（段落/表格/标题层级），pdf 用 pypdf 提取文本检查页数与关键内容，Excel 读回单元格。
9. **输出目录**：一律 `outputs/`，文件名用中文可读名。

## 5. 自检清单（交付前）

- [ ] 输入格式走了正确路线（xlsx 没用 pandoc，PDF 生成没硬试）
- [ ] 转换产物被实际读取验证过（段落/表格/页数/单元格）
- [ ] 中文排版达标（字体/标题层级/表格样式）
- [ ] 文件在 outputs/，文件名可读
- [ ] 生成 PDF 类需求已按 §2 处理（docx→pdf 用 LibreOffice；纯 md 生成 pdf 转达限制）
