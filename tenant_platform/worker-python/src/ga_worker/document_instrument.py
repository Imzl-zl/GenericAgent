"""Inject policy-filtered document gateway tools into the legacy GA handler."""

from __future__ import annotations

import hashlib
import json
import sys
from pathlib import Path
from typing import Any, Callable

from ga_worker.session_files import (
    ensure_session_sandbox,
    normalize_output_name,
    persist_document_artifact,
    read_text_file,
    resolve_under_root,
)

_EXPORT_DOCX_TOOL = {
    "type": "function",
    "function": {
        "name": "export_docx",
        "description": "Generate a DOCX through the secure document service and save it under session outputs/.",
        "parameters": {
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "source_path": {"type": "string"},
                "title": {"type": "string"},
                "content": {"type": "string"},
            },
        },
    },
}

_DOCUMENT_SUBMIT_TOOL = {
    "type": "function",
    "function": {
        "name": "document_job_submit",
        "description": "Submit one typed document operation to the current task document job.",
        "parameters": {
            "type": "object",
            "properties": {
                "operation": {"type": "string", "enum": ["export_docx"]},
                "parameters": {"type": "object"},
            },
            "required": ["operation", "parameters"],
        },
    },
}

_DOCUMENT_CLOSE_TOOL = {
    "type": "function",
    "function": {
        "name": "document_job_close",
        "description": "Close the current task document job after all document operations are complete.",
        "parameters": {"type": "object", "properties": {}},
    },
}


def install_document_tools(
    session: Any,
    task_id: str,
    legacy_mods: dict[str, Any] | None,
) -> Callable[[], None]:
    if legacy_mods is None or session is None or session.document_client is None:
        return lambda: None
    ga_mod = legacy_mods.get("ga")
    agentmain = legacy_mods.get("agentmain")
    if ga_mod is None or agentmain is None:
        return lambda: None
    handler_cls = getattr(ga_mod, "GenericAgentHandler", None)
    if handler_cls is None:
        return lambda: None

    sandbox_root = ensure_session_sandbox(Path(session.overlay_dir).parents[1], session.session_key)
    previous_custom_schema = getattr(agentmain, "_tenant_custom_tools_schema", None)
    methods = {
        "do_export_docx": _make_export_handler(session, task_id, sandbox_root),
        "do_document_job_submit": _make_submit_handler(session, task_id, sandbox_root),
        "do_document_job_close": _make_close_handler(session, task_id),
    }
    previous_methods = {name: getattr(handler_cls, name, None) for name in methods}
    for name, handler in methods.items():
        setattr(handler_cls, name, handler)
    custom = list(previous_custom_schema or [])
    existing = {
        tool.get("function", {}).get("name")
        for tool in custom
        if isinstance(tool, dict)
    }
    for schema in (_EXPORT_DOCX_TOOL, _DOCUMENT_SUBMIT_TOOL, _DOCUMENT_CLOSE_TOOL):
        if schema["function"]["name"] not in existing:
            custom.append(schema)
    agentmain._tenant_custom_tools_schema = custom

    def unwrap() -> None:
        for name, installed in methods.items():
            if getattr(handler_cls, name, None) is not installed:
                continue
            previous = previous_methods[name]
            if previous is None:
                delattr(handler_cls, name)
            else:
                setattr(handler_cls, name, previous)
        if previous_custom_schema is None:
            if hasattr(agentmain, "_tenant_custom_tools_schema"):
                delattr(agentmain, "_tenant_custom_tools_schema")
        else:
            agentmain._tenant_custom_tools_schema = previous_custom_schema

    return unwrap


def _make_export_handler(session: Any, task_id: str, sandbox_root: Path) -> Callable[..., Any]:
    def do_export_docx(self: Any, args: dict[str, Any], response: Any):
        try:
            request_id = _request_id(task_id, args, response)
            parameters = _export_parameters(args, sandbox_root)
            _prepare_document_job(session, task_id)
            download = session.document_client.export_docx(
                task_id=task_id,
                request_id=request_id,
                output_name=parameters["output_name"],
                title=parameters["title"],
                content=parameters["content"],
            )
            rel_path = _persist_download(session, sandbox_root, download)
            yield f"[Document Result] {rel_path}\n"
            return _step_outcome({"status": "success", "path": rel_path})
        except Exception as exc:
            yield f"[Document Error] {exc}\n"
            return _step_outcome(
                {"status": "error", "message": str(exc)},
                next_prompt="Document export failed; report the explicit error instead of assuming success.\n",
            )
    return do_export_docx


def _make_submit_handler(session: Any, task_id: str, sandbox_root: Path) -> Callable[..., Any]:
    def do_document_job_submit(self: Any, args: dict[str, Any], response: Any):
        try:
            request_id = _request_id(task_id, args, response)
            operation = str(args.get("operation") or "").strip()
            if operation != "export_docx":
                raise ValueError(f"unsupported document operation: {operation}")
            raw_parameters = args.get("parameters")
            if not isinstance(raw_parameters, dict):
                raise ValueError("document operation parameters must be an object")
            parameters = _export_parameters(raw_parameters, sandbox_root)
            _prepare_document_job(session, task_id)
            session.document_client.submit(task_id, "document_job_submit", request_id, operation, parameters)
            session.document_client.wait_for_command(task_id, "document_job_submit", request_id)
            download = session.document_client.download_artifact(task_id, "document_job_submit", request_id)
            rel_path = _persist_download(session, sandbox_root, download)
            yield f"[Document Result] {rel_path}\n"
            return _step_outcome({"status": "success", "path": rel_path, "request_id": request_id})
        except Exception as exc:
            yield f"[Document Error] {exc}\n"
            return _step_outcome(
                {"status": "error", "message": str(exc)},
                next_prompt="Document operation failed; report the explicit error instead of assuming success.\n",
            )
    return do_document_job_submit


def _make_close_handler(session: Any, task_id: str) -> Callable[..., Any]:
    def do_document_job_close(self: Any, args: dict[str, Any], response: Any):
        try:
            open_task_id = getattr(session, "document_open_task_id", None)
            if open_task_id is None:
                session.document_client.close(task_id)
            else:
                close_open_document_job(session, task_id)
            yield "[Document Result] job closed\n"
            return _step_outcome({"status": "success", "closed": True})
        except Exception as exc:
            yield f"[Document Error] {exc}\n"
            return _step_outcome(
                {"status": "error", "message": str(exc)},
                next_prompt="Document job close failed; report the explicit error.\n",
            )
    return do_document_job_close


def _prepare_document_job(session: Any, task_id: str) -> None:
    open_task_id = getattr(session, "document_open_task_id", None)
    if open_task_id is not None and open_task_id != task_id:
        close_open_document_job(session, task_id)
    session.document_open_task_id = task_id


def close_open_document_job(session: Any, task_id: str) -> None:
    if session is None:
        return
    client = getattr(session, "document_client", None)
    open_task_id = getattr(session, "document_open_task_id", None)
    if client is None or open_task_id is None:
        return
    client.close(open_task_id)
    session.document_open_task_id = None


def _request_id(task_id: str, args: dict[str, Any], response: Any) -> str:
    index = args.get("_index")
    tool_calls = getattr(response, "tool_calls", None)
    if not isinstance(index, int) or not isinstance(tool_calls, list) or index < 0 or index >= len(tool_calls):
        raise ValueError("document tool call id is unavailable")
    tool_call_id = getattr(tool_calls[index], "id", None)
    if not isinstance(tool_call_id, str) or not tool_call_id.strip():
        raise ValueError("document tool call id is unavailable")
    digest = hashlib.sha256(f"{task_id}\0{tool_call_id.strip()}".encode("utf-8")).hexdigest()
    return f"worker:{digest}"


def _export_parameters(args: dict[str, Any], sandbox_root: Path) -> dict[str, str]:
    output_path = normalize_output_name(str(args.get("path") or args.get("output_name") or "document.docx"))
    content = args.get("content")
    if content is None or str(content).strip() == "":
        source_path = str(args.get("source_path") or "").strip()
        if not source_path:
            raise ValueError("export_docx requires content or source_path")
        content = read_text_file(resolve_under_root(sandbox_root, source_path), max_bytes=1024 * 1024)
    content = str(content)
    if not content.strip() or len(content.encode("utf-8")) > 1024 * 1024:
        raise ValueError("export_docx content must be non-empty and at most 1048576 bytes")
    title = str(args.get("title") or "").strip()
    if len(title.encode("utf-8")) > 4096:
        raise ValueError("export_docx title is too large")
    return {"output_name": Path(output_path).name, "title": title, "content": content}


def _persist_download(session: Any, sandbox_root: Path, download: Any) -> str:
    rel_path = persist_document_artifact(sandbox_root, download.file_name, download.content, download.sha256)
    generated = getattr(session, "generated_output_files", None)
    if not isinstance(generated, list):
        raise ValueError("session generated_output_files is unavailable")
    if rel_path not in generated:
        generated.append(rel_path)
    return rel_path


def _step_outcome(data: dict[str, Any], next_prompt: str = "\n") -> Any:
    step_outcome_cls = getattr(sys.modules.get("agent_loop"), "StepOutcome", None)
    if step_outcome_cls is None:
        return data
    return step_outcome_cls(data, next_prompt=next_prompt, should_exit=False)
