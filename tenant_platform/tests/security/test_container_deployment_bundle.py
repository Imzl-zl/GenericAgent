from __future__ import annotations

from pathlib import Path
import re

import yaml


ROOT = Path(__file__).resolve().parents[3]
COMPOSE_DIR = ROOT / "tenant_platform" / "infra" / "compose"
COMPOSE_FILE = COMPOSE_DIR / "compose.yaml"
ONEPANEL_COMPOSE_FILE = COMPOSE_DIR / "compose.1panel.yaml"


def _compose() -> dict:
    return yaml.safe_load(COMPOSE_FILE.read_text(encoding="utf-8"))


def test_container_bundle_has_required_operator_files() -> None:
    required = {
        ".env.example",
        ".env.production.example",
        "compose.1panel.yaml",
        ".gitignore",
        "README.zh-CN.md",
        "bot-poller.Dockerfile",
        "compose-preflight.sh",
        "compose.yaml",
        "document-runtime-preflight.sh",
        "document-manager.1panel.env.example",
        "env/bot-poller.env.example",
        "env/1panel.env.example",
        "env/platform.env.example",
        "env/postgres.env.example",
        "ga-document-manager-1panel.service",
        "ga-document-manager.service",
        "genericagent-compose.service",
        "nginx.conf",
        "platform.Dockerfile",
        "web.Dockerfile",
    }
    missing = sorted(path for path in required if not (COMPOSE_DIR / path).is_file())
    assert not missing, f"missing container deployment files: {missing}"



def test_production_env_template_separates_digest_images_from_secrets() -> None:
    template = (COMPOSE_DIR / ".env.production.example").read_text(encoding="utf-8")
    for key in (
        "COMPOSE_PROJECT_NAME=genericagent",
        "GA_HTTP_BIND=127.0.0.1",
        "GA_POSTGRES_BIND=127.0.0.1",
        "GA_PLATFORM_IMAGE=REPLACE_WITH_PLATFORM_IMAGE_AT_SHA256",
        "GA_BOT_POLLER_IMAGE=REPLACE_WITH_BOT_POLLER_IMAGE_AT_SHA256",
        "GA_WEB_IMAGE=REPLACE_WITH_WEB_IMAGE_AT_SHA256",
        "GA_POSTGRES_IMAGE=postgres@sha256:",
    ):
        assert key in template


def test_compose_preserves_runtime_and_host_namespace_boundaries() -> None:
    services = _compose()["services"]
    assert "document-manager" not in services
    assert set(services) == {"postgres", "bot-poller", "platform", "web"}

    for name, service in services.items():
        assert service.get("privileged") is not True, name
        assert service.get("network_mode") != "host", name
        assert service.get("pid") != "host", name
        assert service.get("ipc") != "host", name
        for mount in service.get("volumes", []):
            assert "docker.sock" not in str(mount).lower(), (name, mount)
            assert "podman.sock" not in str(mount).lower(), (name, mount)


def test_1panel_profile_is_self_contained_and_excludes_document_manager() -> None:
    profile = yaml.safe_load(ONEPANEL_COMPOSE_FILE.read_text(encoding="utf-8"))
    services = profile["services"]
    assert set(services) == {"postgres", "bot-poller", "platform", "web"}
    assert "document-manager" not in services
    for name, service in services.items():
        assert "build" not in service
        assert "env_file" not in service
        assert service.get("restart") == "unless-stopped"
        assert service.get("privileged") is not True, name
        assert service.get("network_mode") != "host", name
        assert service.get("pid") != "host", name
        assert service.get("ipc") != "host", name
        assert service.get("user") not in (None, "", "0", "0:0", "root"), name
        assert service.get("read_only") is True, name
        assert "ALL" in service.get("cap_drop", []), name
        assert "no-new-privileges:true" in {str(value).lower() for value in service.get("security_opt", [])}
        for mount in service.get("volumes", []):
            assert isinstance(mount, str), (name, mount)
            source = mount.split(":", 1)[0]
            assert source in {"postgres_data", "platform_runtime", "platform_config", "session_files", "bot_media"}, (name, mount)
    assert services["web"]["network_mode"] == "service:platform"


def test_1panel_environment_template_contains_all_required_runtime_values() -> None:
    template = (COMPOSE_DIR / "env" / "1panel.env.example").read_text(encoding="utf-8")
    for key in (
        "GA_PLATFORM_IMAGE=docker.io/zhangl580/genericagent-platform@sha256:5ec7f4ac9793133238b7710757ff91afb0593512d5dfdeb6d051bc10842f421d",
        "GA_BOT_POLLER_IMAGE=docker.io/zhangl580/genericagent-bot-poller@sha256:b61fa498a10318292c53e60cabfdd2495e7b4628b15bce19f7b8ea08313cf9ff",
        "GA_WEB_IMAGE=docker.io/zhangl580/genericagent-web@sha256:d7748b63f015bb216384dba4c515ca5fe6067e6dd2f8659aa984c3fbd331595d",
        "POSTGRES_PASSWORD=CHANGE_ME",
        "DATABASE_URL=postgres://genericagent:CHANGE_ME",
        "BOT_POLLER_API_SECRET=CHANGE_ME",
        "PLATFORM_WEBHOOK_SECRET=CHANGE_ME",
        "LLM_PROXY_CAPABILITY_SIGNING_KEY=CHANGE_ME",
    ):
        assert key in template
    values = dict(line.split("=", 1) for line in template.splitlines() if "=" in line and not line.startswith("#"))
    for key in ("GA_PLATFORM_IMAGE", "GA_BOT_POLLER_IMAGE", "GA_WEB_IMAGE", "GA_POSTGRES_IMAGE"):
        assert re.fullmatch(r"[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}", values[key])
    assert "DOCUMENT_MANAGER_IMAGE" not in values


def test_1panel_document_manager_bundle_targets_the_host_stack() -> None:
    manager_env = (COMPOSE_DIR / "document-manager.1panel.env.example").read_text(encoding="utf-8")
    manager_unit = (COMPOSE_DIR / "ga-document-manager-1panel.service").read_text(encoding="utf-8")
    runtime_preflight = (COMPOSE_DIR / "document-runtime-preflight.sh").read_text(encoding="utf-8")
    assert "DATABASE_URL=postgres://genericagent:CHANGE_ME@127.0.0.1:55432/genericagent?sslmode=disable" in manager_env
    assert "must match GA_POSTGRES_PORT" in manager_env
    assert "DOCUMENT_MANAGER_IMAGE=docker.io/zhangl580/genericagent-document-tool@sha256:a67a176bc046e28d38ef24dc300b51b0066a1275ffa980b9d5d2b846669e7f61" in manager_env
    assert "Requires=genericagent-compose.service" not in manager_unit
    assert "After=network-online.target docker.service" in manager_unit
    assert "User=ga-document" in manager_unit
    assert "NoNewPrivileges=true" in manager_unit
    assert "BindReadOnlyPaths=-/run/user/%U/docker.sock -/run/user/%U/podman/podman.sock" in manager_unit
    assert "/run/docker.sock" in manager_unit
    assert "GA_DOCUMENT_STACK_MODE=1panel" in manager_unit
    assert "GA_1PANEL_PLATFORM_HEALTH_URL=http://127.0.0.1:8088/healthz" in manager_unit
    assert "1panel)" in runtime_preflight
    assert 'curl --fail --silent --show-error "$PLATFORM_HEALTH_URL"' in runtime_preflight
    guide = (COMPOSE_DIR / "README.1panel.zh-CN.md").read_text(encoding="utf-8")
    assert "GA_POSTGRES_PORT" in guide
    assert "Document Manager 镜像" in guide


def test_application_containers_are_hardened_and_non_root() -> None:
    services = _compose()["services"]
    for name, service in services.items():
        assert service.get("restart") == "no", f"{name} must only restart through the systemd preflight owner"
        assert service.get("read_only") is True, name
        assert service.get("user") not in (None, "", "0", "0:0", "root"), name
        assert "ALL" in service.get("cap_drop", []), name
        options = {str(value).lower() for value in service.get("security_opt", [])}
        assert "no-new-privileges:true" in options, name


def test_postgres_and_shared_delivery_volumes_have_explicit_runtime_constraints() -> None:
    services = _compose()["services"]
    postgres = services["postgres"]
    assert postgres.get("user") == "70:70"
    assert postgres.get("pids_limit")
    assert postgres.get("mem_limit")
    assert postgres.get("cpus")
    assert postgres.get("ulimits", {}).get("nofile")

    platform = services["platform"]
    bot = services["bot-poller"]
    assert "10003" in [str(group) for group in platform.get("group_add", [])]
    assert "10003" in [str(group) for group in bot.get("group_add", [])]
    platform_image = (COMPOSE_DIR / "platform.Dockerfile").read_text(encoding="utf-8")
    bot_image = (COMPOSE_DIR / "bot-poller.Dockerfile").read_text(encoding="utf-8")
    for dockerfile in (platform_image, bot_image):
        assert "10003" in dockerfile
        assert "ga-delivery" in dockerfile
        assert "umask 0027" in dockerfile


def test_compose_preflight_checks_root_owned_trusted_configuration() -> None:
    script = (COMPOSE_DIR / "compose-preflight.sh").read_text(encoding="utf-8")
    assert "root:root" in script
    assert "require_trusted_parent_chain" in script
    assert "docker compose --env-file .env -f compose.yaml config --format json" in script
    assert "import json" in script
    assert "import yaml" not in script


def test_only_loopback_ports_are_published_by_default() -> None:
    services = _compose()["services"]
    assert services["platform"]["ports"] == [
        "${GA_HTTP_BIND:-127.0.0.1}:${GA_HTTP_PORT:-8088}:8088"
    ]
    assert services["postgres"]["ports"] == [
        "${GA_POSTGRES_BIND:-127.0.0.1}:${GA_POSTGRES_PORT:-55432}:5432"
    ]
    assert "ports" not in services["bot-poller"]
    assert services["web"]["network_mode"] == "service:platform"


def test_platform_image_contains_worker_policy_and_explicit_migrations() -> None:
    dockerfile = (COMPOSE_DIR / "platform.Dockerfile").read_text(encoding="utf-8")
    assert "tenant_platform/worker-python/pyproject.toml" in dockerfile
    assert "tenant_platform/worker-python/src" in dockerfile
    assert "tenant_platform/contracts/policy/foundation.v1.json" in dockerfile
    assert "tenant_platform/infra/postgres/migrations" in dockerfile
    assert "GA_MIGRATIONS_DIR=/opt/ga/migrations" in dockerfile
    assert "USER 10001:10001" in dockerfile


def test_host_document_manager_keeps_the_dedicated_rootless_boundary() -> None:
    manager_unit = (COMPOSE_DIR / "ga-document-manager.service").read_text(encoding="utf-8")
    compose_unit = (COMPOSE_DIR / "genericagent-compose.service").read_text(encoding="utf-8")
    assert "Requires=genericagent-compose.service" in manager_unit
    assert "User=ga-document" in manager_unit
    assert "BindReadOnlyPaths=-/run/user/%U/docker.sock -/run/user/%U/podman/podman.sock" in manager_unit
    assert "ExecStartPre=+/usr/bin/env GA_DOCUMENT_RUNTIME_PREFLIGHT=1 /usr/bin/bash /opt/genericagent/source/tenant_platform/infra/compose/document-runtime-preflight.sh" in manager_unit
    assert "/run/docker.sock" in manager_unit
    assert "WorkingDirectory=/opt/genericagent/source/tenant_platform/infra/compose" in compose_unit
    assert "PartOf=docker.service" in compose_unit
    assert "ExecStartPre=/usr/bin/env GA_COMPOSE_DEPLOY_PREFLIGHT=1 /usr/bin/bash /opt/genericagent/source/tenant_platform/infra/compose/compose-preflight.sh" in compose_unit
    assert "docker compose" in compose_unit


def test_operator_guide_explains_file_and_command_semantics() -> None:
    guide = (COMPOSE_DIR / "README.zh-CN.md").read_text(encoding="utf-8")
    for required in (
        "为什么不能做成一个镜像",
        "容器里能不能执行命令和操作文件",
        "首次部署",
        "升级",
        "回滚",
        "Document Manager",
        "rootless",
    ):
        assert required in guide
