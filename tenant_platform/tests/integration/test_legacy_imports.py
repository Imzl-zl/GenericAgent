"""Legacy import regression checks run in an isolated Python subprocess."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
_IMPORTED = (
    "agentmain",
    "ga",
    "llmcore",
    "frontends.chatapp_common",
    "frontends.ga_contract_probe",
)
_SENSITIVE_ENV_MARKERS = ("KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL")
_SENTINEL = "legacy-imports-ok"


def _scrubbed_env() -> dict[str, str]:
    return {
        name: value
        for name, value in os.environ.items()
        if not any(marker in name.upper() for marker in _SENSITIVE_ENV_MARKERS)
    }


def test_legacy_modules_import_cleanly_without_configuration_output(tmp_path: Path) -> None:
    module_names = repr(_IMPORTED)
    script = (
        "import importlib, sys\n"
        f"sys.path.insert(0, {str(REPO_ROOT)!r})\n"
        f"sys.path.insert(0, {str(REPO_ROOT / 'frontends')!r})\n"
        f"for name in {module_names}: importlib.import_module(name)\n"
        f"print({_SENTINEL!r})\n"
    )

    completed = subprocess.run(
        [sys.executable, "-I", "-c", script],
        cwd=tmp_path,
        env=_scrubbed_env(),
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=60,
        check=False,
    )

    assert completed.returncode == 0, completed.stderr
    assert completed.stdout == f"{_SENTINEL}\n"
    assert completed.stderr == ""
