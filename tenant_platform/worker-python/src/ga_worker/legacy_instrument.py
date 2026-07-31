"""Legacy GenericAgent module instrumentation helpers.

Extracted from managed_agent.py (B3: file size limit). These stateless helpers
monkeypatch legacy modules (agentmain, ga, agent_loop) to enforce RuntimePolicy:
tool schema filtering, dispatch guard, print byte counter, max_turns, handler seed.

Fixes applied during extraction:
- P-M2: dispatch guard and handler seed now return unwrap callables that restore
  originals, preventing leaks across sessions/tasks.
- P-M5: no hard imports inside function bodies; sys imported at module level.
"""

from __future__ import annotations

import copy
import sys
import threading
from pathlib import Path
from typing import Any, Callable

from ga_worker.limits import ToolPolicy
from ga_worker.session_files import ensure_session_sandbox, resolve_under_root


def prepare_handler_seed(
    agent: Any,
    seed_working: dict[str, Any],
    agent_factory: Any,
    legacy_mods: dict[str, Any] | None,
) -> Callable[[], None]:
    """Ensure new handler receives restored working; clear carryover handler.

    Returns an unwrap callable that restores the original GenericAgentHandler
    class (P-M2: previously leaked the WrappedHandler across sessions).
    """
    agent.handler = None

    if agent_factory is not None and seed_working:
        class _Seed:
            def __init__(self):
                self.working = copy.deepcopy(seed_working)
                self.code_stop_signal: list[int] = []
                self.history_info: list[Any] = []

        agent._adapter_seed_working = copy.deepcopy(seed_working)
        if getattr(agent, "handler", None) is None:
            agent.handler = _Seed()

    original_handler_cls: Any = None
    if legacy_mods and seed_working:
        ga_mod = legacy_mods.get("ga")
        if ga_mod is not None and hasattr(ga_mod, "GenericAgentHandler"):
            original_handler_cls = ga_mod.GenericAgentHandler
            seed = copy.deepcopy(seed_working)

            class WrappedHandler(original_handler_cls):  # type: ignore[misc,valid-type]
                def __init__(self, *args, **kwargs):
                    super().__init__(*args, **kwargs)
                    self.working = copy.deepcopy(seed)

            ga_mod.GenericAgentHandler = WrappedHandler  # type: ignore[assignment]
            agent._adapter_handler_wrapped = original_handler_cls

    def unwrap() -> None:
        if legacy_mods and original_handler_cls is not None:
            ga_mod = legacy_mods.get("ga")
            if ga_mod is not None and getattr(ga_mod, "GenericAgentHandler", None) is not original_handler_cls:
                ga_mod.GenericAgentHandler = original_handler_cls  # type: ignore[assignment]

    return unwrap


def apply_tool_policy(tool_policy: ToolPolicy, legacy_mods: dict[str, Any] | None) -> Any:
    """Filter agentmain.TOOLS_SCHEMA to allowed tools; return previous schema."""
    if legacy_mods is None:
        return None
    agentmain = legacy_mods.get("agentmain")
    if agentmain is None or not hasattr(agentmain, "TOOLS_SCHEMA"):
        return None
    previous = agentmain.TOOLS_SCHEMA
    allowed = tool_policy.allowed_tools
    augmented = list(previous)
    for extra in getattr(agentmain, "_tenant_custom_tools_schema", []) or []:
        if not isinstance(extra, dict):
            continue
        name = extra.get("function", {}).get("name")
        if not name:
            continue
        if any(isinstance(t, dict) and t.get("function", {}).get("name") == name for t in augmented):
            continue
        augmented.append(extra)
    filtered = [
        t
        for t in augmented
        if isinstance(t, dict)
        and t.get("function", {}).get("name") in allowed
    ]
    agentmain.TOOLS_SCHEMA = filtered
    return previous


def restore_tool_schema(previous: Any, legacy_mods: dict[str, Any] | None) -> None:
    if previous is None or legacy_mods is None:
        return
    agentmain = legacy_mods.get("agentmain")
    if agentmain is not None:
        agentmain.TOOLS_SCHEMA = previous


_EXPORT_DOCX_TOOL = {
    "type": "function",
    "function": {
        "name": "export_docx",
        "description": "Generate a .docx file inside the session outputs/ sandbox. Use content or <file_content> in the reply body; optionally set source_path to convert an existing text file.",
        "parameters": {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "relative output path, usually outputs/<name>.docx"},
                "source_path": {"type": "string", "description": "optional relative text source file to convert"},
                "title": {"type": "string", "description": "optional document title"},
                "content": {"type": "string", "description": "optional plain text content; if omitted, the tool reads <file_content> from the assistant reply body"},
            },
        },
    },
}


def install_session_file_sandbox(session: Any, legacy_mods: dict[str, Any] | None) -> Callable[[], None]:
    if legacy_mods is None or session is None:
        return lambda: None
    ga_mod = legacy_mods.get("ga")
    if ga_mod is None:
        return lambda: None
    handler_cls = getattr(ga_mod, "GenericAgentHandler", None)
    if handler_cls is None:
        return lambda: None

    sandbox_root = ensure_session_sandbox(Path(session.overlay_dir).parents[1], session.session_key)
    original_init = getattr(handler_cls, "__init__", None)
    original_get_abs_path = getattr(handler_cls, "_get_abs_path", None)

    def _set_cwd(handler: Any) -> None:
        if handler is not None:
            handler.cwd = str(sandbox_root)

    if callable(original_init):
        def sandboxed_init(self, *args, **kwargs):
            original_init(self, *args, **kwargs)
            _set_cwd(self)
        handler_cls.__init__ = sandboxed_init  # type: ignore[assignment]

    def sandboxed_get_abs_path(self, path):
        raw = (path or "").strip()
        if not raw:
            return ""
        return str(resolve_under_root(sandbox_root, raw))

    handler_cls._get_abs_path = sandboxed_get_abs_path  # type: ignore[assignment]
    _set_cwd(getattr(session.agent, "handler", None))

    def unwrap() -> None:
        if callable(original_init) and getattr(handler_cls, "__init__", None) is sandboxed_init:
            handler_cls.__init__ = original_init  # type: ignore[assignment]
        if original_get_abs_path is not None and getattr(handler_cls, "_get_abs_path", None) is sandboxed_get_abs_path:
            handler_cls._get_abs_path = original_get_abs_path  # type: ignore[assignment]

    return unwrap


def install_global_mcp_tools(session: Any, legacy_mods: dict[str, Any] | None) -> Callable[[], None]:
    """Expose the session's administrator-enabled MCP catalog to every tenant task."""
    if legacy_mods is None or session is None:
        return lambda: None
    ga_mod = legacy_mods.get("ga")
    agentmain = legacy_mods.get("agentmain")
    if ga_mod is None or agentmain is None:
        return lambda: None
    handler_cls = getattr(ga_mod, "GenericAgentHandler", None)
    if handler_cls is None:
        return lambda: None

    catalog = getattr(session, "mcp_tools", None) or {}
    if not isinstance(catalog, dict):
        raise ValueError("session mcp_tools must be a mapping")

    previous_custom_schema = getattr(agentmain, "_tenant_custom_tools_schema", None)
    previous_tool_names = getattr(agentmain, "_tenant_global_mcp_tool_names", None)
    original_methods: dict[str, Any] = {}
    installed_methods: dict[str, Any] = {}
    custom = list(previous_custom_schema or [])
    existing_names = {
        tool.get("function", {}).get("name")
        for tool in custom
        if isinstance(tool, dict)
    }
    prepared: list[tuple[str, dict[str, Any], Any, str]] = []

    # Validate the complete catalog before mutating the global legacy handler.
    for ga_name, binding in catalog.items():
        if not isinstance(ga_name, str) or not isinstance(binding, dict):
            raise ValueError("invalid MCP tool binding")
        schema = binding.get("schema")
        client = binding.get("client")
        remote_name = binding.get("tool_name")
        if (
            not isinstance(schema, dict)
            or schema.get("function", {}).get("name") != ga_name
            or not isinstance(remote_name, str)
            or client is None
        ):
            raise ValueError(f"invalid MCP tool binding for {ga_name}")
        method_name = f"do_{ga_name}"
        if hasattr(handler_cls, method_name):
            raise ValueError(f"MCP tool conflicts with existing handler method: {ga_name}")
        if ga_name in existing_names:
            raise ValueError(f"duplicate custom tool name: {ga_name}")
        prepared.append((method_name, schema, client, remote_name))
        existing_names.add(ga_name)

    def make_mcp_handler(client: Any, remote_name: str) -> Callable[..., Any]:
        def do_mcp_tool(self, args, response):
            try:
                public_args = {
                    key: value for key, value in dict(args).items()
                    if key not in ("_index", "_tool_num")
                }
                result = client.call_tool(remote_name, public_args)
            except Exception as exc:
                yield f"[Status] ❌ MCP 工具调用失败: {exc}\n"
                step_outcome_cls = getattr(sys.modules.get("agent_loop"), "StepOutcome", None)
                if step_outcome_cls is None:
                    raise
                return step_outcome_cls(
                    {"status": "error", "message": str(exc)},
                    next_prompt="MCP tool failed; report the explicit error instead of assuming success.\n",
                    should_exit=False,
                )
            yield f"[MCP Result]\n{result}\n"
            step_outcome_cls = getattr(sys.modules.get("agent_loop"), "StepOutcome", None)
            if step_outcome_cls is None:
                return result
            return step_outcome_cls(result, next_prompt="\n")
        return do_mcp_tool

    for method_name, schema, client, remote_name in prepared:
        do_mcp_tool = make_mcp_handler(client, remote_name)
        original_methods[method_name] = getattr(handler_cls, method_name, None)
        installed_methods[method_name] = do_mcp_tool
        setattr(handler_cls, method_name, do_mcp_tool)
        custom.append(schema)

    agentmain._tenant_custom_tools_schema = custom
    agentmain._tenant_global_mcp_tool_names = frozenset(catalog)

    def unwrap() -> None:
        for method_name, installed in installed_methods.items():
            if getattr(handler_cls, method_name, None) is not installed:
                continue
            original = original_methods[method_name]
            if original is None:
                delattr(handler_cls, method_name)
            else:
                setattr(handler_cls, method_name, original)
        if previous_custom_schema is None:
            if hasattr(agentmain, "_tenant_custom_tools_schema"):
                delattr(agentmain, "_tenant_custom_tools_schema")
        else:
            agentmain._tenant_custom_tools_schema = previous_custom_schema
        if previous_tool_names is None:
            if hasattr(agentmain, "_tenant_global_mcp_tool_names"):
                delattr(agentmain, "_tenant_global_mcp_tool_names")
        else:
            agentmain._tenant_global_mcp_tool_names = previous_tool_names

    return unwrap


def install_dispatch_guard(
    tool_policy: ToolPolicy,
    legacy_mods: dict[str, Any] | None,
) -> Callable[[], None]:
    """Install tool-policy dispatch guard on GenericAgentHandler.

    Returns an unwrap callable that restores the original dispatch method
    (P-M2: previously the guard was never restored, leaking across tasks).
    """
    if legacy_mods is None:
        return lambda: None
    ga_mod = legacy_mods.get("ga")
    agent_loop = legacy_mods.get("agent_loop")
    if ga_mod is None:
        return lambda: None
    handler_cls = getattr(ga_mod, "GenericAgentHandler", None)
    if handler_cls is None:
        return lambda: None

    allowed = tool_policy.allowed_tools

    # Restore original before (re)installing to avoid double-wrapping.
    original_dispatch = getattr(handler_cls, "_adapter_original_dispatch", None)
    if original_dispatch is None:
        original_dispatch = handler_cls.dispatch
    else:
        handler_cls.dispatch = original_dispatch

    def guarded(self, tool_name, args, response, index=0, tool_num=1):
        if tool_name not in allowed and tool_name not in ("no_tool", "bad_json"):
            yield f"tool denied by policy: {tool_name}\n"
            step_outcome_cls = getattr(agent_loop, "StepOutcome", None) if agent_loop is not None else None
            if step_outcome_cls is None:
                al_mod = sys.modules.get("agent_loop")
                step_outcome_cls = getattr(al_mod, "StepOutcome", None) if al_mod is not None else None
            if step_outcome_cls is not None:
                return step_outcome_cls(None, next_prompt=f"tool denied: {tool_name}", should_exit=False)
            return None
        return (yield from original_dispatch(self, tool_name, args, response, index=index, tool_num=tool_num))

    handler_cls.dispatch = guarded  # type: ignore[assignment]
    handler_cls._adapter_original_dispatch = original_dispatch  # type: ignore[attr-defined]
    handler_cls._adapter_dispatch_guard = allowed  # type: ignore[attr-defined]

    def unwrap() -> None:
        if getattr(handler_cls, "dispatch", None) is guarded:
            handler_cls.dispatch = original_dispatch  # type: ignore[assignment]
        for attr in ("_adapter_original_dispatch", "_adapter_dispatch_guard"):
            if hasattr(handler_cls, attr):
                delattr(handler_cls, attr)

    return unwrap


def _make_handler_wrapper(
    count_fn: Callable[[str], bool],
    wrapped: list[tuple[Any, Any]],
) -> Callable[[Any], None]:
    """Build a _wrap_handler closure that instruments handler.print.

    Each wrapped handler is recorded in `wrapped` as (handler, original_print)
    so the caller can restore it later.
    """

    def _wrap_handler(handler: Any) -> None:
        if handler is None or getattr(handler, "_adapter_print_wrapped", False):
            return
        original_print = getattr(handler, "print", None)

        def counted_print(*args, **kwargs):
            text = " ".join(str(a) for a in args)
            if count_fn(text):
                return None
            if callable(original_print):
                return original_print(*args, **kwargs)
            return None

        handler.print = counted_print  # type: ignore[assignment]
        handler._adapter_print_wrapped = True  # type: ignore[attr-defined]
        wrapped.append((handler, original_print))

    return _wrap_handler


def _restore_wrapped_handlers(wrapped: list[tuple[Any, Any]]) -> None:
    """Restore original handler.print and clear wrap markers."""
    for handler, original_print in wrapped:
        if not getattr(handler, "_adapter_print_wrapped", False):
            continue
        try:
            if original_print is not None:
                handler.print = original_print  # type: ignore[assignment]
            elif hasattr(handler, "print"):
                delattr(handler, "print")
        except Exception:
            pass
        try:
            delattr(handler, "_adapter_print_wrapped")
        except Exception:
            handler._adapter_print_wrapped = False  # type: ignore[attr-defined]


def _swap_counted_handler(
    ga_mod: Any,
    count_fn: Callable[[str], bool],
    wrap_handler: Callable[[Any], None],
) -> tuple[Any, Any]:
    """Swap ga_mod.GenericAgentHandler to a counted subclass.

    Returns (original_cls, counted_cls). counted_cls is None when no swap
    occurred (already installed for this count_fn, or no target).
    """
    if ga_mod is None or not hasattr(ga_mod, "GenericAgentHandler"):
        return None, None
    original_cls = ga_mod.GenericAgentHandler
    if getattr(original_cls, "_adapter_print_factory", None) is count_fn:
        return original_cls, None

    class _CountedHandler(original_cls):  # type: ignore[misc,valid-type]
        def __init__(self, *args, **kwargs):
            super().__init__(*args, **kwargs)
            wrap_handler(self)

    _CountedHandler._adapter_print_factory = count_fn  # type: ignore[attr-defined]
    ga_mod.GenericAgentHandler = _CountedHandler  # type: ignore[assignment]
    return original_cls, _CountedHandler


def _swap_watched_agent(
    agent: Any,
    wrap_handler: Callable[[Any], None],
) -> tuple[Any, Any]:
    """Swap agent.__class__ to a watched subclass that wraps handler on assign.

    Returns (original_class, watched_cls). watched_cls is None when no swap
    occurred (already watched, or __class__ reassignment failed).
    """
    original_class = agent.__class__
    if getattr(agent, "_adapter_handler_watch", False):
        return original_class, None
    agent._adapter_handler_watch = True  # type: ignore[attr-defined]
    try:
        class _WatchedAgent(original_class):  # type: ignore[misc,valid-type]
            def __setattr__(self, name, value):
                super().__setattr__(name, value)
                if name == "handler":
                    wrap_handler(value)

        object.__setattr__(agent, "__class__", _WatchedAgent)
        return original_class, _WatchedAgent
    except Exception:
        agent._adapter_wrap_handler = wrap_handler  # type: ignore[attr-defined]
        return original_class, None


def install_handler_print_counter(
    agent: Any,
    count_fn: Callable[[str], bool],
    legacy_mods: dict[str, Any] | None,
) -> Callable[[], None]:
    """Wrap handler.print to count output bytes against the quota.

    Returns an unwrap callable that restores the original agent class, the
    original GenericAgentHandler class, and each wrapped handler's print, so
    the same agent can be reused across tasks without counter leakage
    (P-M2 pattern, matching install_dispatch_guard/install_max_turns).
    """
    wrapped: list[tuple[Any, Any]] = []
    wrap_handler = _make_handler_wrapper(count_fn, wrapped)
    wrap_handler(getattr(agent, "handler", None))

    ga_mod = legacy_mods.get("ga") if legacy_mods else None
    original_handler_cls, counted_cls = _swap_counted_handler(ga_mod, count_fn, wrap_handler)
    original_agent_class, watched_cls = _swap_watched_agent(agent, wrap_handler)

    def unwrap() -> None:
        if watched_cls is not None and agent.__class__ is watched_cls:
            object.__setattr__(agent, "__class__", original_agent_class)
        if (counted_cls is not None and ga_mod is not None
                and getattr(ga_mod, "GenericAgentHandler", None) is counted_cls):
            ga_mod.GenericAgentHandler = original_handler_cls  # type: ignore[assignment]
        _restore_wrapped_handlers(wrapped)
        for attr in ("_adapter_handler_watch", "_adapter_wrap_handler"):
            if hasattr(agent, attr):
                try:
                    delattr(agent, attr)
                except Exception:
                    pass

    return unwrap


def install_max_turns(
    agent: Any,
    max_turns: int,
    legacy_mods: dict[str, Any] | None,
) -> Callable[[], None]:
    """Force RuntimePolicy.max_turns into agent_runner_loop without editing legacy files.

    Returns an unwrap callable that restores original agent_runner_loop in all
    affected modules (P-M2: previously only unwrapped via finally, now clean).
    """
    unwraps: list[Callable[[], None]] = []

    def _wrap_module(mod: Any, attr: str = "agent_runner_loop") -> None:
        if mod is None or not hasattr(mod, attr):
            return
        original = getattr(mod, attr)
        if getattr(original, "_adapter_max_turns_wrapped", False):
            original._adapter_forced_max_turns = max_turns  # type: ignore[attr-defined]
            return

        def wrapped(*args, **kwargs):
            forced = getattr(wrapped, "_adapter_forced_max_turns", max_turns)
            kwargs = dict(kwargs)
            kwargs["max_turns"] = forced
            return original(*args, **kwargs)

        wrapped._adapter_max_turns_wrapped = True  # type: ignore[attr-defined]
        wrapped._adapter_forced_max_turns = max_turns  # type: ignore[attr-defined]
        setattr(mod, attr, wrapped)

        def unwrap() -> None:
            if getattr(mod, attr, None) is wrapped:
                setattr(mod, attr, original)

        unwraps.append(unwrap)

    mods: list[Any] = []
    if legacy_mods:
        mods.extend([legacy_mods.get("agentmain"), legacy_mods.get("agent_loop")])

    for name in ("agentmain", "agent_loop"):
        mod = sys.modules.get(name)
        if mod is not None and mod not in mods:
            mods.append(mod)
    for mod in mods:
        _wrap_module(mod, "agent_runner_loop")
    agent._adapter_max_turns = max_turns

    def unwrap_all() -> None:
        for u in unwraps:
            try:
                u()
            except Exception:
                pass

    return unwrap_all
