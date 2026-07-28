"""Per-session file sandbox helpers and minimal DOCX export."""

from __future__ import annotations

import hashlib
import os
import re
import zipfile
from datetime import datetime, timezone
from pathlib import Path
from xml.sax.saxutils import escape

FILE_MARKER_RE = re.compile(r"\[FILE:([^\]]+)\]")

SESSION_FILES_DIR = "session_files"
ATTACHMENTS_DIR = "attachments"
OUTPUTS_DIR = "outputs"


def session_key_digest(session_key: str) -> str:
    return hashlib.sha256((session_key or "").encode("utf-8")).hexdigest()


def session_sandbox_root(runtime_root: Path, session_key: str) -> Path:
    return Path(runtime_root) / SESSION_FILES_DIR / session_key_digest(session_key)


def ensure_session_sandbox(runtime_root: Path, session_key: str) -> Path:
    root = session_sandbox_root(runtime_root, session_key)
    (root / ATTACHMENTS_DIR).mkdir(parents=True, exist_ok=True)
    (root / OUTPUTS_DIR).mkdir(parents=True, exist_ok=True)
    return root


def resolve_under_root(root: Path, raw_path: str) -> Path:
    root = Path(root).resolve()
    if not raw_path:
        raise ValueError("path is required")
    candidate = Path(raw_path)
    if not candidate.is_absolute():
        candidate = root / candidate
    resolved = candidate.resolve()
    try:
        resolved.relative_to(root)
    except ValueError as exc:
        raise ValueError(f"path escapes session sandbox: {raw_path}") from exc
    return resolved


def normalize_output_name(name: str, default: str = "document.docx") -> str:
    raw = (name or "").strip().replace("\\", "/")
    parts = [part for part in raw.split("/") if part not in ("", ".")]
    safe_parts: list[str] = []
    for part in parts:
        if part == "..":
            continue
        safe = re.sub(r'[:*?"<>|]+', '_', part)
        safe = safe.strip()
        if safe:
            safe_parts.append(safe)
    default_name = re.sub(r'[:*?"<>|]+', '_', (default or "document.docx").split("/")[-1]) or "document.docx"
    if not safe_parts:
        safe_parts = [OUTPUTS_DIR, default_name]
    elif safe_parts[0] != OUTPUTS_DIR:
        safe_parts = [OUTPUTS_DIR, safe_parts[-1]]
    elif len(safe_parts) == 1:
        safe_parts.append(default_name)
    if not safe_parts[-1].lower().endswith('.docx'):
        safe_parts[-1] += '.docx'
    return "/".join(safe_parts)


def _paragraphs_from_text(text: str) -> list[list[str]]:
    normalized = (text or "").replace("\r\n", "\n").replace("\r", "\n").strip()
    if not normalized:
        return [[""]]
    blocks = re.split(r"\n{2,}", normalized)
    paragraphs: list[list[str]] = []
    for block in blocks:
        lines = block.split("\n")
        paragraphs.append(lines)
    return paragraphs


def _xml_run(text: str) -> str:
    if text == "":
        return '<w:r><w:t xml:space="preserve"></w:t></w:r>'
    return f'<w:r><w:t xml:space="preserve">{escape(text)}</w:t></w:r>'


def _xml_paragraph(lines: list[str]) -> str:
    runs: list[str] = []
    for idx, line in enumerate(lines):
        if idx > 0:
            runs.append('<w:r><w:br/></w:r>')
        runs.append(_xml_run(line))
    if not runs:
        runs.append(_xml_run(""))
    return '<w:p>' + ''.join(runs) + '</w:p>'


def build_docx_bytes(text: str, *, title: str = "") -> bytes:
    paragraphs = ''.join(_xml_paragraph(lines) for lines in _paragraphs_from_text(text))
    created = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace('+00:00', 'Z')
    document_xml = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<w:document xmlns:wpc="http://schemas.microsoft.com/office/word/2010/wordprocessingCanvas" '
        'xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006" '
        'xmlns:o="urn:schemas-microsoft-com:office:office" '
        'xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" '
        'xmlns:m="http://schemas.openxmlformats.org/officeDocument/2006/math" '
        'xmlns:v="urn:schemas-microsoft-com:vml" '
        'xmlns:wp14="http://schemas.microsoft.com/office/word/2010/wordprocessingDrawing" '
        'xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" '
        'xmlns:w10="urn:schemas-microsoft-com:office:word" '
        'xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" '
        'xmlns:w14="http://schemas.microsoft.com/office/word/2010/wordml" '
        'xmlns:wpg="http://schemas.microsoft.com/office/word/2010/wordprocessingGroup" '
        'xmlns:wpi="http://schemas.microsoft.com/office/word/2010/wordprocessingInk" '
        'xmlns:wne="http://schemas.microsoft.com/office/word/2006/wordml" '
        'xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape" '
        'mc:Ignorable="w14 wp14">'
        '<w:body>' + paragraphs + '<w:sectPr>'
        '<w:pgSz w:w="11906" w:h="16838"/>'
        '<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="708" w:footer="708" w:gutter="0"/>'
        '<w:cols w:space="708"/>'
        '<w:docGrid w:linePitch="360"/>'
        '</w:sectPr></w:body></w:document>'
    )
    core_xml = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" '
        'xmlns:dc="http://purl.org/dc/elements/1.1/" '
        'xmlns:dcterms="http://purl.org/dc/terms/" '
        'xmlns:dcmitype="http://purl.org/dc/dcmitype/" '
        'xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">'
        f'<dc:title>{escape(title or "GenericAgent Export")}</dc:title>'
        '<dc:creator>GenericAgent</dc:creator>'
        '<cp:lastModifiedBy>GenericAgent</cp:lastModifiedBy>'
        f'<dcterms:created xsi:type="dcterms:W3CDTF">{created}</dcterms:created>'
        f'<dcterms:modified xsi:type="dcterms:W3CDTF">{created}</dcterms:modified>'
        '</cp:coreProperties>'
    )
    app_xml = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" '
        'xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">'
        '<Application>GenericAgent</Application>'
        '</Properties>'
    )
    content_types = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
        '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>'
        '<Default Extension="xml" ContentType="application/xml"/>'
        '<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>'
        '<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>'
        '<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>'
        '<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>'
        '</Types>'
    )
    rels_xml = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
        '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>'
        '<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>'
        '<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>'
        '</Relationships>'
    )
    word_rels_xml = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
        '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>'
        '</Relationships>'
    )
    styles_xml = (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">'
        '<w:style w:type="paragraph" w:default="1" w:styleId="Normal">'
        '<w:name w:val="Normal"/>'
        '<w:qFormat/>'
        '</w:style>'
        '</w:styles>'
    )
    from io import BytesIO

    buf = BytesIO()
    with zipfile.ZipFile(buf, 'w', compression=zipfile.ZIP_DEFLATED) as zf:
        zf.writestr('[Content_Types].xml', content_types)
        zf.writestr('_rels/.rels', rels_xml)
        zf.writestr('docProps/core.xml', core_xml)
        zf.writestr('docProps/app.xml', app_xml)
        zf.writestr('word/document.xml', document_xml)
        zf.writestr('word/_rels/document.xml.rels', word_rels_xml)
        zf.writestr('word/styles.xml', styles_xml)
    return buf.getvalue()


def write_simple_docx(path: Path, text: str, *, title: str = "") -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(build_docx_bytes(text, title=title))


def read_text_file(path: Path) -> str:
    raw = Path(path).read_bytes()
    for enc in ('utf-8', 'utf-8-sig', 'utf-16'):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode('utf-8', errors='replace')


def append_missing_file_markers(body: str, rel_paths: list[str]) -> str:
    normalized_existing = {
        match.group(1).strip().replace("\\", "/")
        for match in FILE_MARKER_RE.finditer(body or "")
    }
    missing: list[str] = []
    for rel_path in rel_paths or []:
        normalized = (rel_path or "").strip().replace("\\", "/")
        if not normalized or normalized in normalized_existing:
            continue
        missing.append(normalized)
    if not missing:
        return body
    suffix = "\n".join(f"[FILE:{path}]" for path in missing)
    base = (body or "").rstrip()
    if not base:
        return suffix + "\n"
    return base + "\n\n" + suffix + "\n"
