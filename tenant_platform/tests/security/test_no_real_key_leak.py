"""Security boundary checks for the transparent LLM credential proxy.

The full Platform integration test captures the live Worker environment,
command line, config/runtime trees, and logs. These fast checks cover the
same boundary without depending on source-line formatting.
"""

from __future__ import annotations

import ast
import os
import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
BACKEND_GO = REPO_ROOT / "tenant_platform" / "backend-go"
WORKER_SRC = REPO_ROOT / "tenant_platform" / "worker-python" / "src"
REAL_KEY_ENV_VARS = ("LLM_PROVIDER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY")
WRITE_METHODS = frozenset(
    {"open", "write", "write_bytes", "write_text", "chmod", "replace"}
)


def _parse(relative_path: str) -> ast.Module:
    path = REPO_ROOT / relative_path
    return ast.parse(path.read_text(encoding="utf-8"), filename=str(path))


def _resolved_strings(node: ast.AST, bindings: dict[str, set[str]]) -> set[str]:
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return {node.value}
    if isinstance(node, ast.Name):
        return bindings.get(node.id, set())
    if isinstance(node, ast.JoinedStr):
        return {
            value.value
            for value in node.values
            if isinstance(value, ast.Constant) and isinstance(value.value, str)
        }
    if isinstance(node, ast.BinOp):
        return _resolved_strings(node.left, bindings) | _resolved_strings(
            node.right, bindings
        )
    if isinstance(node, ast.Attribute):
        return _resolved_strings(node.value, bindings)
    if isinstance(node, ast.Call):
        values: set[str] = set()
        for argument in (*node.args, *[keyword.value for keyword in node.keywords]):
            values.update(_resolved_strings(argument, bindings))
        return values
    return set()


def _string_bindings(tree: ast.Module) -> dict[str, set[str]]:
    bindings: dict[str, set[str]] = {}
    assignments = [
        node for node in ast.walk(tree) if isinstance(node, (ast.Assign, ast.AnnAssign))
    ]
    for _ in range(3):
        for assignment in assignments:
            targets = (
                assignment.targets
                if isinstance(assignment, ast.Assign)
                else [assignment.target]
            )
            value = assignment.value
            if value is None:
                continue
            resolved = _resolved_strings(value, bindings)
            for target in targets:
                if isinstance(target, ast.Name) and resolved:
                    bindings.setdefault(target.id, set()).update(resolved)
    return bindings


def _mykey_writes(tree: ast.Module) -> list[int]:
    bindings = _string_bindings(tree)
    findings: list[int] = []
    for call in (node for node in ast.walk(tree) if isinstance(node, ast.Call)):
        function = call.func
        if isinstance(function, ast.Name) and function.id == "open":
            path_nodes = call.args[:1]
        elif isinstance(function, ast.Attribute) and function.attr in WRITE_METHODS:
            path_nodes = (function.value, *call.args[:1])
        else:
            continue
        paths = set().union(*(_resolved_strings(node, bindings) for node in path_nodes))
        if any("mykey.py" in value.replace("\\", "/").lower() for value in paths):
            findings.append(call.lineno)
    return findings


def _environment_reads(tree: ast.Module, variable: str) -> list[int]:
    findings: list[int] = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Subscript) and isinstance(node.value, ast.Attribute):
            if node.value.attr == "environ" and variable in _resolved_strings(
                node.slice, {}
            ):
                findings.append(node.lineno)
        if not isinstance(node, ast.Call) or not node.args:
            continue
        function = node.func
        if isinstance(function, ast.Attribute) and function.attr in {"get", "getenv"}:
            if variable in _resolved_strings(node.args[0], {}):
                findings.append(node.lineno)
    return findings


def test_platform_harnesses_do_not_write_plaintext_mykey() -> None:
    for relative_path in (
        "tenant_platform/tests/integration/test_foundation_flow.py",
        "tenant_platform/tests/smoke/foundation_smoke.py",
    ):
        findings = _mykey_writes(_parse(relative_path))
        assert not findings, f"{relative_path} writes mykey.py at AST lines {findings}"


def test_ga_and_worker_python_do_not_read_platform_real_keys() -> None:
    paths = [REPO_ROOT / "llmcore.py", *WORKER_SRC.rglob("*.py")]
    for path in paths:
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for variable in REAL_KEY_ENV_VARS:
            findings = _environment_reads(tree, variable)
            assert not findings, f"{path} reads {variable} at AST lines {findings}"


def test_go_worker_environment_and_runtime_config_are_token_only() -> None:
    names = (
        "TestBuildWorkerEnvironmentExcludesInheritedPlatformSecrets",
        "TestWriteTokenOnlyRuntimeConfigContainsTokenAndProxyNotRealKey",
    )
    pattern = "^(" + "|".join(names) + ")$"
    environment = {**os.environ, "TEST_DATABASE_URL": ""}
    completed = subprocess.run(
        [
            "go",
            "test",
            "./internal/infrastructure/worker",
            "./internal/application",
            "-run",
            pattern,
            "-count=1",
            "-v",
        ],
        cwd=BACKEND_GO,
        env=environment,
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )
    output = completed.stdout + completed.stderr
    assert completed.returncode == 0, output
    for name in names:
        assert f"=== RUN   {name}" in output, (
            f"behavior test did not execute: {name}\n{output}"
        )
