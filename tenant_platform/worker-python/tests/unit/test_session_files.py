from __future__ import annotations

import types
from pathlib import Path

from genericagent.worker.v1 import worker_pb2
from ga_worker.legacy_instrument import (
    apply_tool_policy,
    install_dispatch_guard,
    install_global_mcp_tools,
    install_session_file_sandbox,
    restore_tool_schema,
)
from ga_worker.limits import ToolPolicy
from ga_worker.session_files import append_missing_file_markers, ensure_session_sandbox, normalize_output_name, resolve_under_root
from ga_worker.state import PendingTask, TaskRunState
from ga_worker.task_terminal import emit_cancel_or_timeout_terminal, emit_final_terminal


def test_resolve_under_root_rejects_escape(tmp_path: Path):
    root = ensure_session_sandbox(tmp_path, "personal:1")
    resolved = resolve_under_root(root, "outputs/resume.docx")
    assert resolved == root / "outputs" / "resume.docx"

    try:
        resolve_under_root(root, "../escape.txt")
    except ValueError as exc:
        assert "escapes session sandbox" in str(exc)
    else:
        raise AssertionError("expected sandbox escape rejection")


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





def test_session_sandbox_does_not_inject_local_export_docx(tmp_path: Path):
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
        assert names == ["file_read"]

        handler = ga_mod.GenericAgentHandler(None)
        assert "session_files" in Path(handler.cwd).as_posix()
        resolved = Path(handler._get_abs_path("outputs/demo.docx"))
        assert resolved.parts[-2:] == ("outputs", "demo.docx")
        assert "session_files" in resolved.as_posix()
        assert not hasattr(ga_mod.GenericAgentHandler, "do_export_docx")
        assert not resolved.exists()
        assert session.generated_output_files == []
    finally:
        restore_tool_schema(previous, {"ga": ga_mod, "agentmain": agentmain_mod})
        unwrap()


def test_global_mcp_tool_runs_when_tenant_policy_allows_it(tmp_path: Path, monkeypatch):
    import sys

    class StepOutcome:
        def __init__(self, data, next_prompt=None, should_exit=False):
            self.data = data
            self.next_prompt = next_prompt
            self.should_exit = should_exit

    monkeypatch.setitem(sys.modules, "agent_loop", types.SimpleNamespace(StepOutcome=StepOutcome))

    class FakeHandler:
        def __init__(self, *args, **kwargs):
            self.cwd = "./temp"

        def dispatch(self, tool_name, args, response, index=0, tool_num=1):
            method = getattr(self, f"do_{tool_name}", None)
            if method is None:
                yield f"unknown:{tool_name}\n"
                return StepOutcome(None)
            return (yield from method(args, response))

    class FakeClient:
        def call_tool(self, name, arguments):
            assert name == "web_search"
            assert arguments == {"query": "GA"}
            return "search result"

    ga_mod = types.SimpleNamespace(GenericAgentHandler=FakeHandler)
    agentmain_mod = types.SimpleNamespace(TOOLS_SCHEMA=[
        {"type": "function", "function": {"name": "file_read"}},
    ])
    session = types.SimpleNamespace(
        overlay_dir=tmp_path / "runtime" / "s-1" / "legacy-overlay",
        session_key="personal:1",
        agent=types.SimpleNamespace(handler=None),
        generated_output_files=[],
        mcp_tools={
            "exa__web_search": {
                "client": FakeClient(),
                "tool_name": "web_search",
                "schema": {
                    "type": "function",
                    "function": {
                        "name": "exa__web_search",
                        "description": "Search",
                        "parameters": {"type": "object", "properties": {"query": {"type": "string"}}},
                    },
                },
            },
        },
    )
    session.overlay_dir.mkdir(parents=True, exist_ok=True)
    mods = {"ga": ga_mod, "agentmain": agentmain_mod, "agent_loop": sys.modules["agent_loop"]}

    sandbox_unwrap = install_session_file_sandbox(session, mods)
    mcp_unwrap = install_global_mcp_tools(session, mods)
    policy = ToolPolicy(
        version="foundation.session-files.v1",
        allowed_tools=frozenset({"file_read", "export_docx", "exa__web_search"}),
    )
    previous = apply_tool_policy(policy, mods)
    guard_unwrap = install_dispatch_guard(policy, mods)
    try:
        names = [tool["function"]["name"] for tool in agentmain_mod.TOOLS_SCHEMA]
        assert names == ["file_read", "exa__web_search"]

        call = ga_mod.GenericAgentHandler().dispatch("exa__web_search", {"query": "GA"}, None)
        chunks = []
        try:
            while True:
                chunks.append(next(call))
        except StopIteration as stop:
            assert stop.value.data == "search result"
        assert any("search result" in chunk for chunk in chunks)
    finally:
        guard_unwrap()
        restore_tool_schema(previous, mods)
        mcp_unwrap()
        sandbox_unwrap()


def test_global_mcp_install_is_transactional_when_catalog_contains_conflict():
    class FakeHandler:
        def do_conflict__existing(self, args, response):
            return None

    ga_mod = types.SimpleNamespace(GenericAgentHandler=FakeHandler)
    agentmain_mod = types.SimpleNamespace(TOOLS_SCHEMA=[])

    def binding(name: str):
        return {
            "client": object(),
            "tool_name": name,
            "schema": {
                "type": "function",
                "function": {
                    "name": name,
                    "description": name,
                    "parameters": {"type": "object", "properties": {}},
                },
            },
        }

    session = types.SimpleNamespace(mcp_tools={
        "ok__search": binding("ok__search"),
        "conflict__existing": binding("conflict__existing"),
    })
    try:
        install_global_mcp_tools(session, {"ga": ga_mod, "agentmain": agentmain_mod})
    except ValueError as exc:
        assert "conflicts" in str(exc)
    else:
        raise AssertionError("expected MCP handler conflict")

    assert not hasattr(FakeHandler, "do_ok__search")
    assert not hasattr(agentmain_mod, "_tenant_custom_tools_schema")
    assert not hasattr(agentmain_mod, "_tenant_global_mcp_tool_names")


def test_global_mcp_tools_must_intersect_with_tenant_policy(tmp_path: Path, monkeypatch):
    import sys

    class StepOutcome:
        def __init__(self, data, next_prompt=None, should_exit=False):
            self.data = data
            self.next_prompt = next_prompt
            self.should_exit = should_exit

    agent_loop_mod = types.SimpleNamespace(StepOutcome=StepOutcome)
    monkeypatch.setitem(sys.modules, "agent_loop", agent_loop_mod)

    dispatched: list[str] = []

    class FakeHandler:
        def __init__(self, *args, **kwargs):
            self.cwd = "./temp"

        def dispatch(self, tool_name, args, response, index=0, tool_num=1):
            dispatched.append(tool_name)
            method = getattr(self, f"do_{tool_name}", None)
            if method is None:
                yield f"unknown:{tool_name}\n"
                return StepOutcome(None)
            return (yield from method(args, response))

    class FakeClient:
        def call_tool(self, name, arguments):
            assert name == "web_search"
            assert arguments == {"query": "GA"}
            return "search result"

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
        mcp_tools={
            "exa__web_search": {
                "client": FakeClient(),
                "tool_name": "web_search",
                "schema": {
                    "type": "function",
                    "function": {
                        "name": "exa__web_search",
                        "description": "Search",
                        "parameters": {"type": "object", "properties": {"query": {"type": "string"}}},
                    },
                },
            },
        },
    )
    session.overlay_dir.mkdir(parents=True, exist_ok=True)
    mods = {"ga": ga_mod, "agentmain": agentmain_mod, "agent_loop": agent_loop_mod}

    sandbox_unwrap = install_session_file_sandbox(session, mods)
    mcp_unwrap = install_global_mcp_tools(session, mods)
    policy = ToolPolicy(
        version="foundation.session-files.v1",
        allowed_tools=frozenset({"file_read", "export_docx"}),
    )
    previous = apply_tool_policy(policy, mods)
    guard_unwrap = install_dispatch_guard(policy, mods)
    try:
        names = [tool["function"]["name"] for tool in agentmain_mod.TOOLS_SCHEMA]
        assert names == ["file_read"]

        handler = ga_mod.GenericAgentHandler()
        denied = handler.dispatch(
            "exa__web_search",
            {"query": "GA", "_index": 0, "_tool_num": 1},
            None,
        )
        denied_chunks = []
        try:
            while True:
                denied_chunks.append(next(denied))
        except StopIteration:
            pass
        assert any("denied" in chunk for chunk in denied_chunks)
        assert dispatched == []
    finally:
        guard_unwrap()
        restore_tool_schema(previous, mods)
        mcp_unwrap()
        sandbox_unwrap()
