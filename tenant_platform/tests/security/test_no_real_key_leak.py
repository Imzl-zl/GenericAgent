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
SYSTEMD_DIR = REPO_ROOT / "tenant_platform" / "infra" / "systemd"
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


def test_worker_contract_and_runtime_do_not_receive_sophub_credentials() -> None:
    runtime_paths = [
        REPO_ROOT
        / "tenant_platform"
        / "contracts"
        / "proto"
        / "genericagent"
        / "worker"
        / "v1"
        / "worker.proto",
        SYSTEMD_DIR / "ga-worker-manager.service",
        *WORKER_SRC.rglob("*.py"),
    ]
    forbidden = ("sophub_api_key", "sophub endpoint", "fudankw.cn", "/sophub")
    for path in runtime_paths:
        body = path.read_text(encoding="utf-8").lower()
        matches = [token for token in forbidden if token in body]
        assert not matches, f"{path} exposes Sophub control-plane data to Worker: {matches}"


def test_only_document_manager_systemd_unit_holds_container_runtime_access() -> None:
    allowed = {"ga-document-manager.service"}
    access_tokens = (
        "podman.socket",
        "docker.socket",
        "--podman",
        "--docker",
        "--runtime-binary",
        "--worker-runtime",
        "/usr/bin/docker",
        "/usr/bin/podman",
        "DOCUMENT_MANAGER_RUNTIME_BINARY",
    )
    for path in SYSTEMD_DIR.glob("*.service"):
        body = path.read_text(encoding="utf-8")
        matches = [token for token in access_tokens if token in body]
        if path.name not in allowed:
            assert not matches, f"{path} must not hold container runtime access: {matches}"

    platform = (SYSTEMD_DIR / "ga-platform.service").read_text(encoding="utf-8")
    inaccessible = next(
        (line for line in platform.splitlines() if line.startswith("InaccessiblePaths=")),
        "",
    )
    for socket in (
        "/run/docker.sock",
        "/var/run/docker.sock",
        "/run/podman/podman.sock",
        "/var/run/podman/podman.sock",
        "/run/user",
    ):
        assert socket in inaccessible, f"platform must hide runtime endpoint {socket}"
    assert "UnsetEnvironment=DOCKER_HOST CONTAINER_HOST" in platform
    assert "/var/lib/ga/documents" in inaccessible
    assert "/etc/ga/bot-poller.env" in inaccessible

    manager = (SYSTEMD_DIR / "ga-document-manager.service").read_text(encoding="utf-8")
    manager_inaccessible = next(
        line for line in manager.splitlines() if line.startswith("InaccessiblePaths=")
    )
    assert "ProtectHome=true" in manager
    assert "BindReadOnlyPaths=-/run/user/%U/docker.sock -/run/user/%U/podman/podman.sock" in manager
    assert "ReadWritePaths=/var/lib/ga/documents" in manager
    for hidden in (
        "/etc/ga/platform.env", "/etc/ga/document-manager.env", "/var/lib/ga/runtime", "/etc/ga/config", "/opt/ga/worker-python",
        "/run/docker.sock", "/var/run/docker.sock", "/run/podman/podman.sock", "/var/run/podman/podman.sock",
    ):
        assert hidden in manager_inaccessible
    assert "/etc/ga/document-manager.env" in inaccessible

    bot = (SYSTEMD_DIR / "ga-bot-poller.service").read_text(encoding="utf-8")
    assert "EnvironmentFile=-/etc/ga/bot-poller.env" in bot
    assert "BindReadOnlyPaths=-/var/lib/ga/runtime/session_files" in bot
    for hidden in ("/etc/ga/bot-poller.env", "/etc/ga/platform.env", "/etc/ga/document-manager.env", "/var/lib/ga/documents", "/run/user"):
        assert hidden in bot


def test_systemd_services_use_distinct_uids_and_proc_isolation() -> None:
    expected_users = {
        "ga-platform.service": "ga-platform",
        "ga-worker-manager.service": "ga-platform",
        "ga-document-manager.service": "ga-document",
        "ga-bot-poller.service": "ga-bot",
        "ga-llm-proxy.service": "ga-llm",
    }
    for name, user in expected_users.items():
        body = (SYSTEMD_DIR / name).read_text(encoding="utf-8")
        assert f"User={user}" in body
        assert "ProtectProc=invisible" in body
        assert "ProcSubset=pid" in body
    llm = (SYSTEMD_DIR / "ga-llm-proxy.service").read_text(encoding="utf-8")
    assert "ReadWritePaths=/var/lib/ga" not in llm
    assert "/var/lib/ga/documents" in llm
    assert "SupplementaryGroups=ga-delivery" in (SYSTEMD_DIR / "ga-platform.service").read_text(encoding="utf-8")
    assert "SupplementaryGroups=ga-delivery" in (SYSTEMD_DIR / "ga-bot-poller.service").read_text(encoding="utf-8")
    platform_main = (REPO_ROOT / "tenant_platform/backend-go/cmd/platform/main.go").read_text(encoding="utf-8")
    guard = (REPO_ROOT / "tenant_platform/backend-go/internal/infrastructure/processguard/dumpable_linux.go").read_text(encoding="utf-8")
    assert "processguard.DisablePeerInspection()" in platform_main
    assert "unix.PR_SET_DUMPABLE, 0" in guard


def test_document_deployment_assets_are_fail_closed_and_use_shipped_policy() -> None:
    preflight = (
        REPO_ROOT
        / "tenant_platform"
        / "infra"
        / "deploy"
        / "document-platform-preflight.sh"
    ).read_text(encoding="utf-8")
    for required in (
        "cgroup.controllers",
        "not rootless",
        "repository@sha256",
        "systemd-analyze verify",
        "DOCUMENT_MANAGER_IMAGE",
        "foundation.v1.json",
    ):
        assert required in preflight
    assert "|| true" not in preflight
    assert ":latest" not in preflight

    platform = (SYSTEMD_DIR / "ga-platform.service").read_text(encoding="utf-8")
    assert "--policy-file=/opt/ga/policy/foundation.v1.json" in platform
    assert "foundation.yaml" not in platform


def test_document_image_has_single_fixed_non_root_entry_process() -> None:
    dockerfile = (
        REPO_ROOT / "tenant_platform" / "document-image" / "Dockerfile"
    ).read_text(encoding="utf-8")
    final_stage = dockerfile.rsplit("FROM scratch", 1)[-1]
    assert "FROM scratch" in dockerfile
    assert "USER 1000:1000" in final_stage
    assert "ENTRYPOINT" not in final_stage
    assert 'CMD ["/usr/local/bin/ga-document-tool", "idle"]' in final_stage
    assert "/bin/sh" not in final_stage

    manager = (
        REPO_ROOT / "tenant_platform" / "backend-go" / "cmd" / "document-manager" / "main.go"
    ).read_text(encoding="utf-8")
    assert 'Command:        []string{"/usr/local/bin/ga-document-tool", "idle"}' in manager


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
