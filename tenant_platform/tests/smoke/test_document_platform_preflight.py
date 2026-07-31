from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "tenant_platform" / "infra" / "deploy" / "document-platform-preflight.sh"
SYSTEMD = ROOT / "tenant_platform" / "infra" / "systemd"
DIGEST_IMAGE = "registry.example/ga-document@sha256:" + "a" * 64
BASH = shutil.which("bash") or "bash"


def _write_executable(path: Path, body: str) -> None:
    path.write_text("#!/usr/bin/env bash\nset -euo pipefail\n" + body, encoding="utf-8")
    path.chmod(0o755)


@pytest.fixture
def preflight_env(tmp_path: Path) -> dict[str, str]:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    _write_executable(
        fake_bin / "uname",
        "[[ \"${1:-}\" == \"-s\" ]] && echo Linux || echo Linux\n",
    )
    _write_executable(
        fake_bin / "id",
        """
if [[ "${1:-}" == "-u" ]]; then
  case "${2:-}" in
    "") echo 0 ;;
    ga-platform) echo 1001 ;;
    ga-document) echo 1002 ;;
    ga-bot) [[ "${GA_TEST_COLLIDE_UID:-}" == "1" ]] && echo 1001 || echo 1003 ;;
    ga-llm) echo 1004 ;;
    *) exit 1 ;;
  esac
elif [[ "${1:-}" == "-nG" ]]; then
  case "${2:-}" in
    ga-platform|ga-bot) echo "${2} ga-delivery" ;;
    ga-document) echo "${GA_TEST_GROUPS:-ga-document}" ;;
    ga-llm) echo ga-llm ;;
    *) exit 1 ;;
  esac
else
  exit 1
fi
""",
    )
    _write_executable(
        fake_bin / "runuser",
        """
while [[ "$#" -gt 0 && "$1" != "--" ]]; do shift; done
[[ "$#" -gt 0 ]] || exit 1
shift
exec "$@"
""",
    )
    _write_executable(fake_bin / "systemd-analyze", "exit 0\n")
    _write_executable(
        fake_bin / "systemctl",
        """
if [[ "$*" == *FragmentPath* ]]; then
  unit="${2:-}"
  echo "${GA_SYSTEMD_DIR}/${unit}"
elif [[ "$*" == *DropInPaths* ]]; then
  echo "${GA_TEST_DROPINS:-}"
elif [[ "$*" == *LoadState* ]]; then
  echo loaded
else
  exit 1
fi
""",
    )
    _write_executable(
        fake_bin / "stat",
        """
case "$2" in
  %a)
    if [[ -n "${GA_TEST_UNSAFE_PARENT:-}" && "$3" == "$GA_TEST_UNSAFE_PARENT" ]]; then
      echo "${GA_TEST_PARENT_MODE:-777}"
    elif [[ "$3" == "$GA_DOCUMENT_WORK_ROOT" ]]; then
      echo "${GA_TEST_WORK_MODE:-750}"
    elif [[ "$3" == "$GA_EXPECTED_BOT_MEDIA_ROOT" || "$3" == "$GA_EXPECTED_PLATFORM_RUNTIME_ROOT" || "$3" == "${GA_EXPECTED_PLATFORM_RUNTIME_ROOT}/session_files" || "$3" == "$GA_EXPECTED_CONFIG_ROOT" || "$3" == "$GA_RUNTIME_ROOT" ]]; then
      echo 750
    else
      [[ -d "$3" ]] && echo 755 || echo 640
    fi
    ;;
  %U)
    case "$3" in
      "$GA_DOCUMENT_WORK_ROOT"|"$GA_RUNTIME_ROOT") echo ga-document ;;
      "$GA_EXPECTED_BOT_MEDIA_ROOT") echo ga-bot ;;
      "$GA_EXPECTED_PLATFORM_RUNTIME_ROOT"|"${GA_EXPECTED_PLATFORM_RUNTIME_ROOT}/session_files"|"$GA_EXPECTED_CONFIG_ROOT") echo ga-platform ;;
      *) echo root ;;
    esac
    ;;
  %G)
    case "$3" in
      "$GA_DOCUMENT_WORK_ROOT"|"$GA_RUNTIME_ROOT") echo ga-document ;;
      "$GA_EXPECTED_BOT_MEDIA_ROOT"|"${GA_EXPECTED_PLATFORM_RUNTIME_ROOT}/session_files") echo ga-delivery ;;
      "$GA_EXPECTED_PLATFORM_RUNTIME_ROOT"|"$GA_EXPECTED_CONFIG_ROOT"|"${GA_ENV_ROOT}/platform.env") echo ga-platform ;;
      "${GA_ENV_ROOT}/document-manager.env") echo ga-document ;;
      "${GA_ENV_ROOT}/bot-poller.env") echo ga-bot ;;
      *) echo root ;;
    esac
    ;;
  *) exit 1 ;;
esac
""",
    )
    _write_executable(fake_bin / "realpath", "echo \"$1\"\n")
    _write_executable(
        fake_bin / "docker",
        """
if [[ "$1" == "info" && "$3" == *SecurityOptions* ]]; then
  echo '["name=rootless","name=seccomp"]'
elif [[ "$1" == "info" && "$3" == *CgroupVersion* ]]; then
  echo '2'
elif [[ "$1" == "info" && "$3" == *MemoryLimit* ]]; then
  echo 'true true true true'
elif [[ "$1" == "image" && "$2" == "inspect" ]]; then
  exit 0
else
  exit 1
fi
""",
    )

    cgroup = tmp_path / "cgroup"
    cgroup.mkdir()
    (cgroup / "cgroup.controllers").write_text("cpu memory pids\n", encoding="utf-8")
    work = tmp_path / "documents"
    work.mkdir()
    bot_media = tmp_path / "bot-media"
    bot_media.mkdir()
    platform_runtime = tmp_path / "platform-runtime"
    (platform_runtime / "session_files").mkdir(parents=True)
    config_root = tmp_path / "config"
    config_root.mkdir()
    runtime_root = tmp_path / "runtime-user"
    runtime_root.mkdir()
    env_root = tmp_path / "etc-ga"
    env_root.mkdir()
    platform_env = env_root / "platform.env"
    platform_env.write_bytes(
        (
            "\n".join(
                (
                    "DATABASE_URL=postgres://local/test",
                    "BOT_TOKEN_KEY=" + "b" * 64,
                    "PLATFORM_DEV_TOKEN=" + "p" * 32,
                    "PLATFORM_DEV_USER_ID=1",
                    f"GA_RUNTIME_DIR={platform_runtime.as_posix()}",
                    f"GA_CONFIG_ROOT={config_root.as_posix()}",
                    "GA_LEGACY_ROOT=/opt/ga/legacy",
                    "LLM_PROXY_CAPABILITY_SIGNING_KEY=" + "s" * 32,
                    "BOT_POLLER_URL=http://127.0.0.1:8090",
                    "BOT_POLLER_API_SECRET=" + "a" * 32,
                    "PLATFORM_WEBHOOK_URL=http://127.0.0.1:8080/v1/im/webhook",
                    "PLATFORM_WEBHOOK_SECRET=" + "w" * 32,
                    "DOCUMENT_POOL_MAX_ACTIVE_HARD=1",
                )
            )
            + "\n"
        ).encode("utf-8")
    )
    manager_env = env_root / "document-manager.env"
    manager_env.write_bytes(
        (
            "\n".join(
                (
                    "DATABASE_URL=postgres://local/test",
                    "DOCUMENT_MANAGER_OWNER=manager-1",
                    f"DOCUMENT_MANAGER_WORK_ROOT={work.as_posix()}",
                    "DOCUMENT_MANAGER_RUNTIME_BINARY=docker",
                    "DOCUMENT_MANAGER_IMAGE=" + DIGEST_IMAGE,
                    "DOCUMENT_MANAGER_SECCOMP_PROFILE=builtin",
                    "DOCUMENT_MANAGER_UID=1000",
                    "DOCUMENT_MANAGER_GID=1000",
                    "DOCUMENT_MANAGER_MEMORY_BYTES=268435456",
                    "DOCUMENT_MANAGER_CPU_PERIOD=100000",
                    "DOCUMENT_MANAGER_CPU_QUOTA=50000",
                    "DOCUMENT_MANAGER_PIDS_LIMIT=64",
                    "DOCUMENT_MANAGER_TMPFS_BYTES=67108864",
                    "DOCUMENT_POOL_MAX_ACTIVE_HARD=1",
                    "DOCUMENT_MANAGER_CLAIM_LEASE=30s",
                    "DOCUMENT_MANAGER_HEARTBEAT_INTERVAL=10s",
                    "DOCUMENT_MANAGER_POLL_INTERVAL=500ms",
                    "DOCUMENT_MANAGER_COMMAND_POLL_INTERVAL=250ms",
                    "DOCUMENT_MANAGER_SHUTDOWN_TIMEOUT=30s",
                    f"XDG_RUNTIME_DIR={runtime_root.as_posix()}",
                    f"DOCKER_HOST=unix://{runtime_root.as_posix()}/docker.sock",
                )
            )
            + "\n"
        ).encode("utf-8")
    )
    bot_env = env_root / "bot-poller.env"
    bot_env.write_bytes(
        (
            "\n".join(
                (
                    "BOT_POLLER_LISTEN=127.0.0.1:8090",
                    f"BOT_POLLER_MEDIA_DIR={bot_media.as_posix()}",
                    "BOT_POLLER_API_SECRET=" + "a" * 32,
                    "PLATFORM_WEBHOOK_SECRET=" + "w" * 32,
                )
            )
            + "\n"
        ).encode("utf-8")
    )
    platform_env.chmod(0o600)
    manager_env.chmod(0o600)
    bot_env.chmod(0o600)

    unit_dir = tmp_path / "systemd"
    shutil.copytree(SYSTEMD, unit_dir)
    policy = tmp_path / "foundation.v1.json"
    policy.write_text("{}\n", encoding="utf-8")
    return {
        **os.environ,
        "PATH": f"{fake_bin.as_posix()}{os.pathsep}{os.environ['PATH']}",
        "GA_DOCUMENT_DEPLOY_PREFLIGHT": "1",
        "GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST": "1",
        "GA_CGROUP_ROOT": cgroup.as_posix(),
        "GA_RUNTIME_ROOT": runtime_root.as_posix(),
        "GA_EXPECTED_WORK_ROOT": work.as_posix(),
        "GA_EXPECTED_BOT_MEDIA_ROOT": bot_media.as_posix(),
        "GA_EXPECTED_PLATFORM_RUNTIME_ROOT": platform_runtime.as_posix(),
        "GA_EXPECTED_CONFIG_ROOT": config_root.as_posix(),
        "GA_PLATFORM_USER": "ga-platform",
        "GA_DOCUMENT_USER": "ga-document",
        "GA_BOT_USER": "ga-bot",
        "GA_LLM_USER": "ga-llm",
        "GA_DELIVERY_GROUP": "ga-delivery",
        "GA_DOCUMENT_RUNTIME_BINARY": "docker",
        "GA_DOCUMENT_SMOKE_IMAGE": DIGEST_IMAGE,
        "GA_SYSTEMD_DIR": unit_dir.as_posix(),
        "GA_POLICY_FILE": policy.as_posix(),
        "GA_ENV_ROOT": env_root.as_posix(),
        "GA_DOCUMENT_WORK_ROOT": work.as_posix(),
    }


def _run(env: dict[str, str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [BASH, SCRIPT.as_posix()],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
        timeout=90,
        check=False,
    )


def test_document_platform_preflight_accepts_complete_rootless_host(preflight_env: dict[str, str]) -> None:
    completed = _run(preflight_env)
    assert completed.returncode == 0, completed.stdout + completed.stderr
    assert "target host is ready" in completed.stdout
    assert "secret-value" not in completed.stdout + completed.stderr


def test_document_platform_preflight_rejects_runtime_group_membership(preflight_env: dict[str, str]) -> None:
    completed = _run({**preflight_env, "GA_TEST_GROUPS": "ga-document docker"})
    assert completed.returncode != 0
    assert "unexpected supplementary group docker" in completed.stderr


def test_document_platform_preflight_rejects_non_distinct_service_uids(preflight_env: dict[str, str]) -> None:
    completed = _run({**preflight_env, "GA_TEST_COLLIDE_UID": "1"})
    assert completed.returncode != 0
    assert "distinct UIDs" in completed.stderr


def test_document_platform_preflight_rejects_unsafe_work_root(preflight_env: dict[str, str]) -> None:
    completed = _run({**preflight_env, "GA_TEST_WORK_MODE": "777"})
    assert completed.returncode != 0
    assert "work root" in completed.stderr


def test_document_platform_preflight_rejects_unsafe_parent_directory(preflight_env: dict[str, str]) -> None:
    parent = Path(preflight_env["GA_DOCUMENT_WORK_ROOT"]).parent.as_posix()
    completed = _run({**preflight_env, "GA_TEST_UNSAFE_PARENT": parent})
    assert completed.returncode != 0
    assert "parent directory" in completed.stderr


def test_document_platform_preflight_rejects_untraversable_parent_directory(preflight_env: dict[str, str]) -> None:
    parent = Path(preflight_env["GA_DOCUMENT_WORK_ROOT"]).parent.as_posix()
    completed = _run({
        **preflight_env,
        "GA_TEST_UNSAFE_PARENT": parent,
        "GA_TEST_PARENT_MODE": "700",
    })
    assert completed.returncode != 0
    assert "traversable" in completed.stderr


def test_document_platform_preflight_rejects_systemd_dropins(preflight_env: dict[str, str]) -> None:
    completed = _run({**preflight_env, "GA_TEST_DROPINS": "/etc/systemd/system/ga-platform.service.d/override.conf"})
    assert completed.returncode != 0
    assert "drop-in" in completed.stderr


@pytest.mark.parametrize(
    ("key", "shadow", "message"),
    (
        ("GA_SYSTEMD_DIR", "/tmp/shadow-systemd", "GA_SYSTEMD_DIR must be /etc/systemd/system"),
        ("GA_POLICY_FILE", "/tmp/shadow-policy.json", "unit's fixed policy path"),
        ("GA_ENV_ROOT", "/tmp/shadow-env", "GA_ENV_ROOT must be /etc/ga"),
    ),
)
def test_document_platform_preflight_rejects_shadow_deployment_paths(
    preflight_env: dict[str, str], key: str, shadow: str, message: str,
) -> None:
    env = {
        **preflight_env,
        "GA_SYSTEMD_DIR": "/etc/systemd/system",
        "GA_POLICY_FILE": "/opt/ga/policy/foundation.v1.json",
        "GA_ENV_ROOT": "/etc/ga",
        key: shadow,
    }
    env.pop("GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST")
    completed = _run(env)
    assert completed.returncode != 0
    assert message in completed.stderr


def test_document_platform_preflight_rejects_tagged_image(preflight_env: dict[str, str]) -> None:
    env = {**preflight_env, "GA_DOCUMENT_SMOKE_IMAGE": "registry.example/ga-document:latest"}
    completed = _run(env)
    assert completed.returncode != 0
    assert "repository@sha256" in completed.stderr


def test_document_platform_preflight_rejects_rootful_runtime(preflight_env: dict[str, str], tmp_path: Path) -> None:
    docker = Path(preflight_env["PATH"].split(os.pathsep)[0]) / "docker"
    _write_executable(
        docker,
        """
if [[ "$1" == "info" && "$3" == *SecurityOptions* ]]; then
  echo '["name=seccomp"]'
elif [[ "$1" == "info" && "$3" == *CgroupVersion* ]]; then
  echo '2'
else
  exit 0
fi
""",
    )
    completed = _run(preflight_env)
    assert completed.returncode != 0
    assert "not rootless" in completed.stderr
