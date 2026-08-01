from __future__ import annotations

from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[3]
COMPOSE_DIR = ROOT / "tenant_platform" / "infra" / "compose"
COMPOSE_FILE = COMPOSE_DIR / "compose.yaml"


def _compose() -> dict:
    return yaml.safe_load(COMPOSE_FILE.read_text(encoding="utf-8"))


def _env_values() -> dict[str, str]:
    values: dict[str, str] = {}
    for line in (COMPOSE_DIR / ".env.example").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            key, value = line.split("=", 1)
            values[key] = value
    return values


def test_compose_bundle_has_one_complete_operator_entrypoint() -> None:
    required = {
        ".env.example",
        ".gitignore",
        "README.zh-CN.md",
        "bot-poller.Dockerfile",
        "compose.yaml",
        "document-manager.Dockerfile",
        "document-manager-entrypoint.sh",
        "nginx.conf",
        "platform.Dockerfile",
        "web.Dockerfile",
    }
    missing = sorted(path for path in required if not (COMPOSE_DIR / path).is_file())
    assert not missing, f"missing Compose deployment files: {missing}"
    assert not (COMPOSE_DIR / "env").exists()
    assert not (COMPOSE_DIR / "secrets").exists()
    assert not (ROOT / "tenant_platform" / "infra" / "deploy").exists()
    assert not (ROOT / "tenant_platform" / "infra" / "systemd").exists()


def test_one_env_template_contains_every_compose_value() -> None:
    values = _env_values()
    for key in (
        "COMPOSE_PROJECT_NAME",
        "GA_HTTP_BIND",
        "GA_HTTP_PORT",
        "GA_POSTGRES_BIND",
        "GA_POSTGRES_PORT",
        "GA_PLATFORM_IMAGE",
        "GA_BOT_POLLER_IMAGE",
        "GA_WEB_IMAGE",
        "GA_DOCUMENT_MANAGER_IMAGE",
        "GA_POSTGRES_IMAGE",
        "POSTGRES_USER",
        "POSTGRES_PASSWORD",
        "POSTGRES_DB",
        "DATABASE_URL",
        "BOT_TOKEN_KEY",
        "BOT_POLLER_API_SECRET",
        "PLATFORM_WEBHOOK_SECRET",
        "PLATFORM_DEV_TOKEN",
        "LLM_PROXY_CAPABILITY_SIGNING_KEY",
        "DOCUMENT_MANAGER_IMAGE",
        "DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME",
        "DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE",
    ):
        assert key in values
    assert values["GA_HTTP_BIND"] == "127.0.0.1"
    assert values["GA_POSTGRES_BIND"] == "127.0.0.1"
    assert values["DOCUMENT_GATEWAY_BASE_URL"] == "http://127.0.0.1:8080/v1/document"
    assert values["DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME"] == "true"
    assert values["DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE"] == "true"
    assert values["POSTGRES_PASSWORD"] in values["DATABASE_URL"]


def test_compose_starts_document_manager_but_only_it_receives_docker_socket() -> None:
    services = _compose()["services"]
    assert set(services) == {"postgres", "bot-poller", "platform", "web", "document-manager"}

    for name, service in services.items():
        assert "env_file" not in service, name
        assert service.get("restart") == "unless-stopped", name
        assert service.get("privileged") is not True, name
        assert service.get("network_mode") != "host", name
        assert service.get("pid") != "host", name
        assert service.get("ipc") != "host", name
        assert service.get("read_only") is True, name
        assert "ALL" in service.get("cap_drop", []), name
        options = {str(value).lower() for value in service.get("security_opt", [])}
        assert "no-new-privileges:true" in options, name
        mounts = [str(mount).lower() for mount in service.get("volumes", [])]
        has_runtime_socket = any("docker.sock" in mount or "podman.sock" in mount for mount in mounts)
        assert has_runtime_socket is (name == "document-manager"), name

    manager = services["document-manager"]
    assert manager["build"]["dockerfile"] == "tenant_platform/infra/compose/document-manager.Dockerfile"
    assert manager["user"] == "0:0"
    assert "document_work:/var/lib/ga/documents" in manager["volumes"]
    assert "/var/run/docker.sock:/var/run/docker.sock" in manager["volumes"]
    assert manager["environment"]["DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME"] == "${DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME}"
    assert manager["environment"]["DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE"] == "${DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE}"
    assert manager["environment"]["GA_MIGRATIONS_DIR"] == "${GA_MIGRATIONS_DIR}"
    assert manager["depends_on"]["postgres"]["condition"] == "service_healthy"

    assert services["postgres"]["environment"] == {
        "POSTGRES_USER": "${POSTGRES_USER}",
        "POSTGRES_PASSWORD": "${POSTGRES_PASSWORD}",
        "POSTGRES_DB": "${POSTGRES_DB}",
    }
    assert services["web"]["network_mode"] == "service:platform"


def test_application_configuration_uses_named_volumes() -> None:
    volumes = _compose()["volumes"]
    assert set(volumes) == {"postgres_data", "platform_runtime", "platform_config", "session_files", "bot_media", "document_work"}
    assert all(not value for value in volumes.values())


def test_only_loopback_application_ports_are_published() -> None:
    services = _compose()["services"]
    assert services["platform"]["ports"] == ["${GA_HTTP_BIND:-127.0.0.1}:${GA_HTTP_PORT:-8088}:8088"]
    assert services["postgres"]["ports"] == ["${GA_POSTGRES_BIND:-127.0.0.1}:${GA_POSTGRES_PORT:-55432}:5432"]
    assert "ports" not in services["bot-poller"]
    assert "ports" not in services["document-manager"]


def test_platform_image_contains_worker_policy_and_explicit_migrations() -> None:
    dockerfile = (COMPOSE_DIR / "platform.Dockerfile").read_text(encoding="utf-8")
    for required in (
        "tenant_platform/worker-python/pyproject.toml",
        "tenant_platform/worker-python/src",
        "tenant_platform/contracts/policy/foundation.v1.json",
        "tenant_platform/infra/postgres/migrations",
        "GA_MIGRATIONS_DIR=/opt/ga/migrations",
        "USER 10001:10001",
    ):
        assert required in dockerfile


def test_document_manager_image_builds_the_document_tool_and_runs_the_manager() -> None:
    dockerfile = (COMPOSE_DIR / "document-manager.Dockerfile").read_text(encoding="utf-8")
    entrypoint = (COMPOSE_DIR / "document-manager-entrypoint.sh").read_text(encoding="utf-8")
    for required in (
        "./cmd/document-manager",
        "tenant_platform/document-image/Dockerfile",
        "tenant_platform/infra/postgres/migrations",
        "golang:1.22-bookworm@sha256:",
        "docker@sha256:",
        "/opt/ga/bin/document-manager",
    ):
        assert required in dockerfile
    for required in (
        "docker image inspect",
        "docker build",
        "DOCUMENT_MANAGER_IMAGE",
        "exec /opt/ga/bin/document-manager",
    ):
        assert required in entrypoint


def test_operator_guide_explains_the_complete_two_file_workflow() -> None:
    guide = (COMPOSE_DIR / "README.zh-CN.md").read_text(encoding="utf-8")
    for required in (
        "compose.yaml",
        ".env",
        "配置并启动",
        "docker compose up -d --build",
        "document-manager",
        "/var/run/docker.sock",
        "命名卷",
        "DOCUMENT_GATEWAY_BASE_URL",
        "docker compose down -v",
    ):
        assert required in guide
