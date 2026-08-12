"""User-facing reply text cleaning for tenant deliveries.

The legacy agent's display stream is a process transcript: it contains turn
markers (``LLM Running (Turn N) ...`` / ``Turn N ...``), tool-call trace lines
(``🛠️ name(args)``), the model's working-memory ``<summary>`` blocks, and
provider error fragments (``!!!Error: ...``). TUI/CLI frontends show all of
it by design; IM tenant deliveries must only show user-facing reply text.

Architecture note (2026-08-12 输出分层): the primary fix is agent_loop's
verbose layering — non-verbose streams contain *no* turn markers / tool
traces, so this module is a pure defense layer for (a) overlay-pinned older
agent versions and (b) belt-and-braces idempotence. Known limitation:
delta-split transcript artifacts (marker/trace split across two 'next'
chunks by the >30-char flush) are not fully removable — acceptable because
new agents never emit them in non-verbose mode.

Semantic boundary: cleaning assumes the model never emits literal
``<summary>...</summary>`` (or turn-marker-shaped text) inside user-facing
reply content; such content is stripped by design. Keep both layers'
assumptions in sync when changing match rules.

Worker-side cleaning (this module) applies to both the streaming chunk text
and the terminal result body, so the typewriter frames and the final delivery
stay consistent. Cleaning is text-only: chunk ``turn`` fields, display history
timing, and digest computation all operate on the same cleaned payload
(worker-internal consistency is preserved).

Match rules track agent_loop.py output formats:
  * turn marker: ``\\n{Turn N ...}\\n\\n`` (task_dir) or
    ``\\nLLM Running (Turn N) ...\\n\\n`` (no task_dir), optional ``**`` bold
    when verbose (kept defensive; worker forces verbose=False).
  * tool trace: non-verbose ``🛠️ name(args)`` line; verbose multi-line
    ``🛠️ Tool: ...`` block is not produced by the worker path.
  * summary: ``<summary>...</summary>`` (model working-memory snapshot,
    required by the system prompt, not user-facing).
  * provider error fragment: ``!!!Error: ...`` (internal failure text; the
    platform terminal path reports errors via its own error channel).
"""

from __future__ import annotations

import re

# Turn marker: \nTurn 3 ...\n\n / \nLLM Running (Turn 3) ...\n\n, optional
# ** bold wrapper (verbose). Single-line match; followed by blank line.
_TURN_MARKER_RE = re.compile(
    r"\n\*{0,2}(?:LLM Running \(Turn \d+\)|Turn \d+) \.\.\.\*{0,2}\n\n"
)
# Tool-call trace line (non-verbose): 🛠️ name(args)\n
_TOOL_TRACE_RE = re.compile(r"\n🛠️ [^\n]*\n")
# Model working-memory summary block (required by system prompt, not
# user-facing). Non-greedy, case-insensitive, may span lines.
_SUMMARY_RE = re.compile(r"<summary>[\s\S]*?</summary>", re.IGNORECASE)
# Provider/LLM error fragment inline (HTTP 503 etc). Removed as internal
# failure text (with its trailing newline); task errors surface via the
# platform terminal error path.
_ERROR_FRAGMENT_RE = re.compile(r"!!!Error: [^\n]+\n?")
# Collapse 3+ consecutive newlines (markers/traces leave gaps).
_BLANK_RUN_RE = re.compile(r"\n{3,}")


def clean_reply_text(text: str) -> str:
    """Strip internal process transcript artifacts from user-facing text.

    Idempotent and safe on already-clean text. Returns '' for pure-tool
    turns (callers skip empty chunks).
    """
    if not text:
        return ""
    text = _SUMMARY_RE.sub("", text)
    text = _TURN_MARKER_RE.sub("\n", text)
    text = _TOOL_TRACE_RE.sub("\n", text)
    text = _ERROR_FRAGMENT_RE.sub("", text)
    text = _BLANK_RUN_RE.sub("\n\n", text)
    return text.strip()
