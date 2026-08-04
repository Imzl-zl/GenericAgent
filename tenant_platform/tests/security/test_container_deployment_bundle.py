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
        "ga-runner.Dockerfile",
        "llm-proxy.Dockerfile",
        "sandbox-manager.Dockerfile",
        "nginx.conf",
        "platform.Dockerfile",
        "reset-dev.sh",
        "web.Dockerfile",
    }
    missing = sorted(path for path in required if not (COMPOSE_DIR / path).is_file())
    assert not missing, f"missing Compose deployment files: {missing}"
    assert not (COMPOSE_DIR / "env").exists()
    assert not (COMPOSE_DIR / "secrets").exists()
    assert not (ROOT / "tenant_platform" / "infra" / "deploy").exists()
    assert not (ROOT / "tenant_platform" / "infra" / "systemd").exists()
    # document 系统已整体删除(方案 §8)。
    assert not (COMPOSE_DIR / "document-manager.Dockerfile").exists()
    assert not (COMPOSE_DIR / "document-manager-entrypoint.sh").exists()


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
        "GA_WORKER_EXECUTION_MODE",
        "GA_RUNNER_IMAGE",
        "GA_RUNNER_MAX_ACTIVE",
        "GA_RUNNER_IDLE_TTL",
        "GA_RUNNER_TASK_TIMEOUT",
        "GA_RUNNER_MEMORY_BYTES",
        "GA_RUNNER_CPU_QUOTA",
        "GA_RUNNER_PIDS_LIMIT",
        "GA_LLM_PROXY_ADDR",
    ):
        assert key in values, f"missing {key}"
    assert values["GA_HTTP_BIND"] == "127.0.0.1"
    assert values["GA_POSTGRES_BIND"] == "127.0.0.1"
    assert values["POSTGRES_PASSWORD"] in values["DATABASE_URL"]


def test_compose_starts_six_services_and_only_sandbox_manager_receives_docker_socket() -> None:
    services = _compose()["services"]
    # ga-runner 是 scale: 0 服务(只构建不启动), 不算常驻服务。
    assert set(services) == {"postgres", "bot-poller", "platform", "web", "llm-proxy", "sandbox-manager", "ga-runner"}
    assert services["ga-runner"].get("scale") == 0, "ga-runner must be scale: 0 (built but not started by up)"
    assert "build" in services["ga-runner"], "ga-runner must be buildable via docker compose build"

    for name, service in services.items():
        assert "env_file" not in service, name
        # ga-runner 是 scale: 0 服务(restart: no), 其余常驻服务必须自动重启。
        if name == "ga-runner":
            assert service.get("restart") == "no", name
            continue
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
        assert has_runtime_socket is (name == "sandbox-manager"), name

    manager = services["sandbox-manager"]
    assert manager["build"]["dockerfile"] == "tenant_platform/infra/compose/sandbox-manager.Dockerfile"
    assert "/var/run/docker.sock:/var/run/docker.sock" in manager["volumes"]
    assert manager["environment"]["GA_RUNNER_IMAGE"] == "${GA_RUNNER_IMAGE:-ga-runner:local}"
    # 最小权限(审查): Manager 不依赖数据库, 不得持有 DB 凭据/网络/卷。
    assert "DATABASE_URL" not in manager["environment"]
    assert "depends_on" not in manager
    assert "database" not in manager.get("networks", [])
    assert not any("platform_runtime" in str(vol) for vol in manager.get("volumes", []))

    assert services["postgres"]["environment"] == {
        "POSTGRES_USER": "${POSTGRES_USER}",
        "POSTGRES_PASSWORD": "${POSTGRES_PASSWORD}",
        "POSTGRES_DB": "${POSTGRES_DB}",
    }
    # round9 审查: web 独立网络命名空间(不再 network_mode: service:platform)——
    # 旧拓扑让 Nginx 在 runner-control 可达的 Platform 容器内监听 8088, Runner
    # 可访问完整用户/管理 API; 新拓扑 web 只接入 application 网络, Runner 不可达。
    web = services["web"]
    assert "network_mode" not in web
    assert set(web.get("networks", [])) == {"application"}
    assert web["ports"] == ["${GA_HTTP_BIND:-127.0.0.1}:${GA_HTTP_PORT:-8088}:8088"]
    # Platform 不再发布宿主端口; 容器内只有 loopback 8080 与 capability 保护 8082。
    assert "ports" not in services["platform"]
    platform_networks = set(services["platform"].get("networks", []))
    assert "runner-control" in platform_networks
    # Platform 必须注入稳定 instance ID(round9: task 去重唯一键依赖)。
    assert services["platform"]["environment"]["GA_PLATFORM_INSTANCE_ID"] == "${GA_PLATFORM_INSTANCE_ID:-platform-1}"
    # 交付 spool: Platform rw + Poller ro 共享卷。
    assert any("delivery_spool:/var/lib/ga/delivery-spool" == str(v) for v in services["platform"].get("volumes", []))
    assert any("delivery_spool:/var/lib/ga/delivery-spool:ro" == str(v) for v in services["bot-poller"].get("volumes", []))

    # llm-proxy 仅内部网络, 不映射宿主端口(方案 §7)。
    llm_proxy = services["llm-proxy"]
    assert "ports" not in llm_proxy
    networks = set(llm_proxy.get("networks", []))
    # 审查 C2: llm-proxy 是唯一持有出站能力的服务(database/runner-control
    # internal + llm-egress 出站); Runner/Platform 不得接入 llm-egress。
    assert networks == {"database", "runner-control", "llm-egress"}
    for other in ("platform", "sandbox-manager", "bot-poller", "web"):
        assert "llm-egress" not in services[other].get("networks", []), other


def test_application_configuration_uses_named_volumes() -> None:
    volumes = _compose()["volumes"]
    assert set(volumes) == {
        "postgres_data", "platform_runtime", "platform_config",
        "session_files", "bot_media", "runner_workspaces",
        "delivery_spool",
        # round10 审查(B1c): 主 API unix socket 共享卷(nginx 只读挂载代理)。
        "platform_sock",
    }
    # runner_workspaces 显式 name: sandbox-manager 需以 daemon 可解析的卷名
    # 做 volume-subpath 挂载(方案 §7); 其余卷保持默认声明。
    for name, value in volumes.items():
        if name == "runner_workspaces":
            assert value == {"name": "runner_workspaces"}
        else:
            assert not value


def test_only_loopback_application_ports_are_published() -> None:
    services = _compose()["services"]
    # round9 审查: 宿主 8088 由独立 web 容器发布(nginx), Platform 不再暴露端口。
    assert services["web"]["ports"] == ["${GA_HTTP_BIND:-127.0.0.1}:${GA_HTTP_PORT:-8088}:8088"]
    assert "ports" not in services["platform"]
    assert services["postgres"]["ports"] == ["${GA_POSTGRES_BIND:-127.0.0.1}:${GA_POSTGRES_PORT:-55432}:5432"]
    assert "ports" not in services["bot-poller"]
    assert "ports" not in services["llm-proxy"]
    assert "ports" not in services["sandbox-manager"]


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
    assert "DOCUMENT_GATEWAY_BASE_URL" not in dockerfile


def test_runner_image_is_readonly_nonroot_with_memory_template() -> None:
    dockerfile = (COMPOSE_DIR / "ga-runner.Dockerfile").read_text(encoding="utf-8")
    for required in (
        "GA_DISABLE_HOST_BROWSER=1",
        "USER 10002:10002",
        "memory-template",
        "/ga/legacy/memory",
        "/ga/runner-state",
    ):
        assert required in dockerfile


def test_operator_guide_explains_the_complete_two_file_workflow() -> None:
    guide = (COMPOSE_DIR / "README.zh-CN.md").read_text(encoding="utf-8")
    for required in (
        "compose.yaml",
        ".env",
        "配置并启动",
        "docker compose up -d --build",
        "sandbox-manager",
        "/var/run/docker.sock",
        "命名卷",
        "GA_RUNNER_MAX_ACTIVE",
        "docker compose down -v",
    ):
        assert required in guide
    assert "document-manager" not in guide
