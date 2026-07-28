from __future__ import annotations

import types
from pathlib import Path
import zipfile

from genericagent.worker.v1 import worker_pb2
from ga_worker.legacy_instrument import apply_tool_policy, install_session_file_sandbox, restore_tool_schema
from ga_worker.limits import ToolPolicy
from ga_worker.session_files import append_missing_file_markers, ensure_session_sandbox, normalize_output_name, resolve_under_root, write_simple_docx
from ga_worker.state import PendingTask, TaskRunState
from ga_worker.task_terminal import emit_final_terminal


def test_resolve_under_root_and_write_docx(tmp_path: Path):
    root = ensure_session_sandbox(tmp_path, "personal:1")
    resolved = resolve_under_root(root, "outputs/resume.docx")
    assert resolved == root / "outputs" / "resume.docx"

    try:
        resolve_under_root(root, "../escape.txt")
    except ValueError as exc:
        assert "escapes session sandbox" in str(exc)
    else:
        raise AssertionError("expected sandbox escape rejection")

    write_simple_docx(resolved, "第一段\n\n第二段", title="简历")
    assert resolved.is_file()
    with zipfile.ZipFile(resolved) as zf:
        names = set(zf.namelist())
        assert "word/document.xml" in names
        assert "[Content_Types].xml" in names


def test_normalize_output_name_preserves_outputs_prefix():
    assert normalize_output_name("outputs/resume.docx") == "outputs/resume.docx"
    assert normalize_output_name("resume") == "outputs/resume.docx"
    assert normalize_output_name("outputs/subdir/resume") == "outputs/subdir/resume.docx"



def test_append_missing_file_markers_idempotent():
    body = "搞定了"
    with_marker = append_missing_file_markers(body, ["outputs/a.docx", "outputs/b.docx"])
    assert "[FILE:outputs/a.docx]" in with_marker
    assert "[FILE:outputs/b.docx]" in with_marker
    again = append_missing_file_markers(with_marker, ["outputs/a.docx"])
    assert again.count("[FILE:outputs/a.docx]") == 1



def test_emit_final_terminal_appends_generated_output_markers():
    recorded: dict[str, str] = {}

    class FakeAdapter:
        def __init__(self):
            self._session = types.SimpleNamespace(generated_output_files=["outputs/demo.docx"])

        def _terminal(self, task_id, status, *, user_message="", error_code=None, result_body=None):
            return worker_pb2.Terminal(task_id=task_id, status=status, user_message=user_message)

        def _record_completed(self, task, term, result_body, display_history, agent):
            recorded["body"] = result_body

    task = worker_pb2.TaskEnvelope(task_id="t1", session_key="personal:1")
    state = TaskRunState(
        pending=PendingTask(task_id="t1", request=worker_pb2.ExecuteTaskRequest()),
        agent=object(),
        final_body="搞定",
    )
    events = list(emit_final_terminal(FakeAdapter(), task, state))
    assert len(events) == 1
    assert "[FILE:outputs/demo.docx]" in state.final_body
    assert "[FILE:outputs/demo.docx]" in recorded["body"]



def test_session_sandbox_injects_export_docx_tool(tmp_path: Path):
    class FakeHandler:
        def __init__(self, *args, **kwargs):
            self.cwd = "./temp"

    ga_mod = types.SimpleNamespace(GenericAgentHandler=FakeHandler)
    agentmain_mod = types.SimpleNamespace(TOOLS_SCHEMA=[
        {"type": "function", "function": {"name": "file_read"}},
        {"type": "function", "function": {"name": "update_working_checkpoint"}},
    ])
    session = types.SimpleNamespace(
        overlay_dir=tmp_path / "runtime" / "s-1" / "legacy-overlay",
        session_key="personal:1",
        agent=types.SimpleNamespace(handler=None),
        generated_output_files=[],
    )
    session.overlay_dir.mkdir(parents=True, exist_ok=True)

    unwrap = install_session_file_sandbox(session, {"ga": ga_mod, "agentmain": agentmain_mod})
    previous = apply_tool_policy(
        ToolPolicy(version="foundation.session-files.v1", allowed_tools=frozenset({"file_read", "export_docx"})),
        {"ga": ga_mod, "agentmain": agentmain_mod},
    )
    try:
        names = [t["function"]["name"] for t in agentmain_mod.TOOLS_SCHEMA]
        assert names == ["file_read", "export_docx"]

        handler = ga_mod.GenericAgentHandler(None)
        assert "session_files" in Path(handler.cwd).as_posix()
        resolved = Path(handler._get_abs_path("outputs/demo.docx"))
        assert resolved.parts[-2:] == ("outputs", "demo.docx")
        assert "session_files" in resolved.as_posix()
        assert hasattr(ga_mod.GenericAgentHandler, "do_export_docx")

        gen = handler.do_export_docx({"path": "outputs/demo.docx", "content": "hello"}, types.SimpleNamespace(content=""))
        out = list(gen)
        assert any("已生成 Word 文件" in chunk for chunk in out)
        assert resolved.is_file()
        assert session.generated_output_files == ["outputs/demo.docx"]
    finally:
        restore_tool_schema(previous, {"ga": ga_mod, "agentmain": agentmain_mod})
        unwrap()
