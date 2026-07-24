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
from typing import Any, Callable

from ga_worker.limits import ToolPolicy


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
    filtered = [
        t
        for t in previous
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


def install_handler_print_counter(
    agent: Any,
    count_fn: Callable[[str], bool],
    legacy_mods: dict[str, Any] | None,
) -> None:
    """Wrap handler.print to count output bytes against the quota."""

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

    _wrap_handler(getattr(agent, "handler", None))

    if legacy_mods:
        ga_mod = legacy_mods.get("ga")
        if ga_mod is not None and hasattr(ga_mod, "GenericAgentHandler"):
            original_cls = ga_mod.GenericAgentHandler
            if getattr(original_cls, "_adapter_print_factory", None) is not count_fn:
                base_init = original_cls.__init__

                def init_with_counter(self, *args, **kwargs):
                    base_init(self, *args, **kwargs)
                    _wrap_handler(self)

                original_cls.__init__ = init_with_counter  # type: ignore[method-assign]
                original_cls._adapter_print_factory = count_fn  # type: ignore[attr-defined]

    # Scripted agents often set handler inside run().
    if not getattr(agent, "_adapter_handler_watch", False):
        agent._adapter_handler_watch = True
        try:
            def watching_setattr(name, value, _orig=object.__setattr__):
                _orig(agent, name, value)
                if name == "handler":
                    _wrap_handler(value)

            object.__setattr__(agent, "__class__", type(agent.__class__.__name__, (agent.__class__,), {
                "__setattr__": lambda self, n, v: watching_setattr(n, v),
            }))
        except Exception:
            agent._adapter_wrap_handler = _wrap_handler


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
