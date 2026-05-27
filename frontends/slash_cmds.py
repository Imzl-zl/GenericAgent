"""Slash-command prompt builders + scheduler-task discovery.

Goal of this module: keep TUI files (tuiapp_v2.py / tui_v3.py) thin. They only
need to forward `/update`, `/autorun`, `/morphling`, `/goal`, `/hive`
to the corresponding `build_*_prompt(args)` here, and ask
`list_scheduler_tasks()` / `start_scheduler_task()` for the `/scheduler` picker.

Design (per user 2026-05-27):
- All non-/scheduler commands are *prompt injection*: we craft a system-style
  request and feed it to the main agent as a normal user message (the TUI is
  free to display the raw `/cmd ...` as the visible bubble).  This keeps the
  agent in-session, lets it use every tool/SOP it normally would, and means
  this file owns zero LLM logic.
- `/scheduler` is the only exception — it touches local FS state directly via
  `sche_tasks/*.json` and the existing scheduler daemon, no LLM needed.
- All prompts deliberately *name* the relevant SOP file so the agent re-reads
  it before acting (per CONSTITUTION rule 2: SOP-first).

This module has zero TUI imports — both frontends can depend on it without
either depending on the other.
"""
from __future__ import annotations

import json
import os
import sys
import subprocess
import time
from pathlib import Path
from typing import Optional


# Repo root = parent of frontends/.  Avoid hard-coding; both TUIs live next to
# this file and share the same anchor.
_ROOT = Path(__file__).resolve().parent.parent


# ----- prompt builders (pure functions, no I/O) ---------------------------
# SOP paths are written inline as literal strings in each builder below: a
# literal is self-documenting and locally readable, and a stale path is a
# zero-radius failure (the prompt is a hint to an intelligent agent, which
# re-reads the dir / asks if a SOP moved) — so we deliberately do NOT wrap it
# in a registry + existence-check machinery.

def _tail(args_text: str, label: str = "额外指示") -> str:
    """Append user-supplied args after a slash command as a free-form suffix.

    User pattern (2026-05-27): the base prompt is a fixed injection that names
    the SOP path; anything the user types after `/cmd ` is appended verbatim so
    they can add per-invocation hints (e.g. `/morphling https://github.com/...`
    or `/goal 调研 X，预算 50k token`).
    """
    extra = (args_text or "").strip()
    return f"\n\n{label}: {extra}" if extra else ""


def build_update_prompt(args_text: str = "") -> str:
    # Faithfully follow the user's own wording (2026-05-27):
    # "git pull 更新一下 GA 你自己，官方渠道 https://github.com/Lsdefine/GenericAgent,
    #  自动合并解决冲突,优先上游分支,本地修改代码也进行保留但不进行 commit."
    return (
        "请你 git pull 更新一下 GA 你自己，官方渠道 "
        "https://github.com/Lsdefine/GenericAgent ，"
        "自动合并解决冲突，优先上游分支，本地修改代码也进行保留但不进行 commit。"
        f"{_tail(args_text)}"
    )


def build_autorun_prompt(args_text: str = "") -> str:
    return (
        "请进入「自主探索 / autonomous 模式」：先读 "
        "memory/autonomous_operation_sop.md。"
        "全程自驱，不可逆 / 高风险动作先 ask_user ，"
        "结案给一份简明回执（做了什么 / 产物在哪 / 下一步）。"
        f"{_tail(args_text, '任务种子')}"
    )


def build_morphling_prompt(args_text: str = "") -> str:
    return (
        "请启用 Morphling 模式吞噬 / 蒸馏外部项目到本仓库：先读 "
        "memory/morphling_sop.md。"
        "没有目标先 ask_user 取 GitHub 仓库 / 本地路径 / 能力描述。"
        f"{_tail(args_text, '目标技能/仓库')}"
    )


def build_goal_prompt(args_text: str = "") -> str:
    return (
        "请进入 Goal 模式：先读 memory/goal_mode_sop.md。"
        "若未给目标，先 ask_user 一次性问清：一句话目标 + condition 约束。"
        f"{_tail(args_text, '用户目标')}"
    )


def build_hive_prompt(args_text: str = "") -> str:
    return (
        "请进入 Goal Hive 模式（多 worker 协作版 goal）：先读 "
        "memory/goal_hive_sop.md。"
        "集群目标 / worker 配额 / 终止条件未明确时先 ask_user 补齐再启动。"
        f"{_tail(args_text, '集群目标')}"
    )


# ----- /scheduler reflect-task discovery + launch -------------------------

def list_reflect_tasks() -> list[dict]:
    """Return [{name, path, doc}] for every reflect/*.py task script.

    `doc` is the module docstring's first line (best-effort) so the picker can
    show a one-liner next to each name.  Empty list if reflect/ doesn't exist.
    """
    out: list[dict] = []
    refl = _ROOT / "reflect"
    if not refl.is_dir():
        return out
    for p in sorted(refl.glob("*.py")):
        if p.name.startswith("_"):
            continue
        doc = ""
        try:
            # Cheap docstring sniff: read first ~40 lines, look for """...""".
            head = p.read_text(encoding="utf-8", errors="ignore").splitlines()[:40]
            joined = "\n".join(head)
            for q in ('"""', "'''"):
                i = joined.find(q)
                if i != -1:
                    j = joined.find(q, i + 3)
                    if j != -1:
                        doc = joined[i + 3:j].strip().splitlines()[0].strip()
                        break
        except Exception:
            pass
        out.append({"name": p.stem, "path": str(p), "doc": doc})
    return out


# ----- hub.pyw parity: every launchable service ---------------------------

_HUB_EXCLUDES = {"goal_mode.py", "chatapp_common.py", "tuiapp.py"}


def _sniff_doc(p) -> str:
    """Best-effort first line of a module docstring (cheap ~40-line read)."""
    try:
        head = p.read_text(encoding="utf-8", errors="ignore").splitlines()[:40]
        joined = "\n".join(head)
        for q in ('"""', "'''"):
            i = joined.find(q)
            if i != -1:
                j = joined.find(q, i + 3)
                if j != -1:
                    body = joined[i + 3:j].strip()
                    if body:
                        return body.splitlines()[0].strip()
    except Exception:
        pass
    return ""


def list_launchable_services() -> list[dict]:
    """Mirror hub.pyw's discover_services() so `/scheduler` shows the *same*
    set of launchable services as the GUI launcher.

    Sources (hub.pyw EXCLUDES = goal_mode.py / chatapp_common.py / tuiapp.py):
      • reflect/*.py   (not '_'-prefixed, not excluded)
          → cmd = [python, agentmain.py, --reflect, reflect/<f>]
      • frontends/*app*.py (not excluded)
          → 'stapp' → `python -m streamlit run … --server.headless=true`
            others   → `python frontends/<f>`

    Returns [{name, cmd, doc, kind}] where `name` is the hub-style path
    ('reflect/foo.py' / 'frontends/bar.py') and doubles as the picker value.
    """
    out: list[dict] = []
    refl = _ROOT / "reflect"
    if refl.is_dir():
        for p in sorted(refl.glob("*.py")):
            if p.name.startswith("_") or p.name in _HUB_EXCLUDES:
                continue
            rel = "reflect/" + p.name
            out.append({
                "name": rel,
                "cmd": [sys.executable, "agentmain.py", "--reflect", rel],
                "doc": _sniff_doc(p),
                "kind": "reflect",
            })
    fe = _ROOT / "frontends"
    if fe.is_dir():
        for p in sorted(fe.glob("*.py")):
            if "app" not in p.name or p.name in _HUB_EXCLUDES:
                continue
            rel = "frontends/" + p.name
            if "stapp" in p.name:
                cmd = [sys.executable, "-m", "streamlit", "run", rel,
                       "--server.headless=true"]
            else:
                cmd = [sys.executable, rel]
            out.append({"name": rel, "cmd": cmd, "doc": _sniff_doc(p),
                        "kind": "frontend"})
    return out


def start_service(name: str) -> tuple[bool, str]:
    """Launch a service from list_launchable_services(), detached & window-less
    (CONSTITUTION rule 14: creationflags at the launch layer only, never via
    subprocess.Popen monkeypatch).

    `name` accepts the hub-style path ('reflect/foo.py') or a bare reflect stem
    ('foo') for backward-compat with `/scheduler start <stem>`.
    """
    svcs = list_launchable_services()
    svc = next((s for s in svcs if s["name"] == name), None)
    if svc is None:  # bare reflect stem fallback
        cand = "reflect/" + name + ".py"
        svc = next((s for s in svcs if s["name"] == cand), None)
    if svc is None:
        return False, f"未知服务: {name}"
    try:
        flags = 0
        if os.name == "nt":
            flags = 0x00000200 | 0x08000000  # NEW_PROCESS_GROUP | NO_WINDOW
        proc = subprocess.Popen(
            svc["cmd"],
            cwd=str(_ROOT),
            creationflags=flags,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            close_fds=True,
        )
        # Poll-and-confirm: if the child dies immediately (bad path, import
        # error, port-in-use, etc) Popen still returns happily — without this
        # check the picker would tick "✅ started" while nothing is running,
        # which is exactly the bug#4 the user hit.  0.4s is the smallest
        # window that catches "explodes at import" without making the UI
        # feel laggy on healthy starts.
        time.sleep(0.4)
        rc = proc.poll()
        if rc is not None:
            return False, f"启动失败 (退出码 {rc}): {svc['name']}"
        return True, f"已启动 {svc['name']} (pid={proc.pid})"
    except Exception as e:
        return False, f"启动失败: {type(e).__name__}: {e}"


# ----- running-state introspection (bug#4) --------------------------------
# Why psutil cmdline-scan instead of a launched-by-us pid registry?
#   • Services launched by a previous TUI run, or by hub.pyw, must also be
#     recognised — otherwise /scheduler would happily start a duplicate.
#   • A registry tied to this process dies when the TUI restarts, but the
#     services keep running (CREATE_NEW_PROCESS_GROUP).  Cmdline scan is the
#     only single source of truth across launchers, surviving restarts.
# Trade-off: it costs ~30ms per /scheduler open, and matches by cmdline tail,
# so two checkouts of GA can collide.  We accept that — running two GAs out
# of two clones is already an unsupported configuration.

def _match_service(cmdline: list[str], svc: dict) -> bool:
    """Does this OS process belong to `svc`?  Match on the trailing script
    arg (`reflect/foo.py` for reflect tasks, `frontends/bar.py` for apps),
    which is invariant across `python` vs `pythonw` vs venv shims."""
    if not cmdline:
        return False
    rel = svc["name"]  # 'reflect/foo.py' | 'frontends/bar.py'
    if svc["kind"] == "reflect":
        # agentmain.py --reflect reflect/foo.py
        has_main = any("agentmain.py" in (a or "") for a in cmdline)
        has_rel = any(rel.replace("/", os.sep) in (a or "") or rel in (a or "")
                      for a in cmdline)
        return has_main and has_rel
    # frontend: either `python frontends/foo.py` or `python -m streamlit run frontends/stapp.py …`
    return any(rel.replace("/", os.sep) in (a or "") or rel in (a or "")
               for a in cmdline)


def running_services() -> dict[str, int]:
    """Return {service_name: pid} for every launchable service currently
    alive on this host.  Empty dict if psutil isn't installed (degrades to
    "/scheduler can't show running marks" rather than crashing the TUI).
    """
    try:
        import psutil  # type: ignore
    except Exception:
        return {}
    svcs = list_launchable_services()
    out: dict[str, int] = {}
    me = os.getpid()
    for proc in psutil.process_iter(["pid", "name", "cmdline"]):
        try:
            if proc.info["pid"] == me:
                continue
            nm = (proc.info.get("name") or "").lower()
            # cheap pre-filter: only python-ish processes
            if "python" not in nm and "py.exe" not in nm:
                continue
            cmd = proc.info.get("cmdline") or []
            for svc in svcs:
                if svc["name"] in out:
                    continue
                if _match_service(cmd, svc):
                    out[svc["name"]] = proc.info["pid"]
                    break
        except Exception:
            # psutil races (proc died mid-iter) are normal; skip silently.
            continue
    return out


def stop_service(name: str) -> tuple[bool, str]:
    """Terminate the service `name` if running.  Returns (ok, message).

    Sends SIGTERM-equivalent (Popen.terminate on Windows = TerminateProcess),
    waits up to 3s, then escalates to kill.  Also reaps obvious children
    (e.g. `python -m streamlit` spawns the actual streamlit worker) so we
    don't leave orphans behind.
    """
    try:
        import psutil  # type: ignore
    except Exception:
        return False, "未安装 psutil，无法停止服务"
    running = running_services()
    pid = running.get(name)
    if pid is None:
        return False, f"{name} 未在运行"
    try:
        parent = psutil.Process(pid)
        kids = parent.children(recursive=True)
        for p in [parent, *kids]:
            try:
                p.terminate()
            except Exception:
                pass
        gone, alive = psutil.wait_procs([parent, *kids], timeout=3.0)
        for p in alive:
            try:
                p.kill()
            except Exception:
                pass
        return True, f"已停止 {name} (pid={pid})"
    except psutil.NoSuchProcess:
        return True, f"{name} 已退出"
    except Exception as e:
        return False, f"停止失败: {type(e).__name__}: {e}"


def list_scheduler_tasks() -> list[dict]:
    """Return [{name, path, schedule, enabled}] for every sche_tasks/*.json.

    Used by the /scheduler picker so users can also toggle traditional cron
    tasks, not just reflect.* scripts.
    """
    out: list[dict] = []
    sd = _ROOT / "sche_tasks"
    if not sd.is_dir():
        return out
    for p in sorted(sd.glob("*.json")):
        try:
            data = json.loads(p.read_text(encoding="utf-8"))
        except Exception:
            data = {}
        out.append({
            "name": p.stem,
            "path": str(p),
            "schedule": data.get("schedule") or data.get("cron") or data.get("every") or "",
            "enabled": bool(data.get("enabled", True)),
        })
    return out


def start_reflect_task(name: str) -> tuple[bool, str]:
    """Spawn `python reflect/<name>.py` detached.  Returns (ok, message).

    Detached because reflect tasks are long-running; we don't want them to die
    with the TUI.  On Windows we use CREATE_NEW_PROCESS_GROUP|CREATE_NO_WINDOW
    so no console pops up (per CONSTITUTION rule 14: only at launch layer, no
    monkeypatching subprocess.Popen).
    """
    script = _ROOT / "reflect" / f"{name}.py"
    if not script.is_file():
        return False, f"reflect/{name}.py 不存在"
    try:
        flags = 0
        if os.name == "nt":
            flags = 0x00000200 | 0x08000000  # NEW_PROCESS_GROUP | NO_WINDOW
        subprocess.Popen(
            [sys.executable, str(script)],
            cwd=str(_ROOT),
            creationflags=flags,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            close_fds=True,
        )
        return True, f"已启动 reflect/{name}.py"
    except Exception as e:
        return False, f"启动失败: {type(e).__name__}: {e}"


# ----- dispatch table for the TUI to register against ---------------------

# (cmd, arg_hint, desc)  — kept identical between v2 and v3 so the palette
# stays consistent across frontends.
PALETTE_ENTRIES: list[tuple[str, str, str]] = [
    ("/update",    "[note]",    "git pull 更新 GA 仓库并报告影响面"),
    ("/autorun",   "[seed]",    "进入 autonomous_operation 自主模式"),
    ("/morphling", "[target]",  "启用 Morphling 蒸馏 / 吞噬外部技能"),
    ("/goal",      "[goal]",    "进入 Goal 模式（需 condition 约束）"),
    ("/hive",      "[target]",  "进入 Hive 多 worker 协作模式"),
    ("/scheduler", "",          "多选启动 reflect 任务 / 查看 cron"),
]


def prompt_for(cmd: str, args_text: str) -> Optional[str]:
    """Return the injected user-message for a given slash command, or None if
    the command isn't one of ours (e.g. /scheduler — handled by TUI directly).
    """
    table = {
        "/update":    build_update_prompt,
        "/autorun":   build_autorun_prompt,
        "/morphling": build_morphling_prompt,
        "/goal":      build_goal_prompt,
        "/hive":      build_hive_prompt,
    }
    fn = table.get(cmd)
    return fn(args_text) if fn else None
