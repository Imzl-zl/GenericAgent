"""Worker process entrypoint: load policy, bind gRPC, inject ManagedAgentAdapter."""

from __future__ import annotations

import argparse
import os
import signal
import sys
import threading
import time
from pathlib import Path

import grpc

from ga_worker.limits import CapabilityRegistry
from ga_worker.managed_agent import ManagedAgentAdapter
from ga_worker.rpc_server import add_worker_servicer


REQUIRED_ENV = (
    "GA_CONFIG_ROOT",
    "GA_LEGACY_ROOT",
    "GA_RUNTIME_DIR",
    "GA_POLICY_FILE",
)


def _require_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise SystemExit(f"missing required environment variable: {name}")
    return value


def _infer_workspace_roots(legacy_root: Path) -> None:
    """容器 Runner 形态(方案 §4/§5): 工作区 memory/temp 挂载在固定位置。
    仅当 GA_LEGACY_ROOT 是容器内固定路径 /ga/legacy(由 Manager 挂载)时
    才推断工作区根; loopback 开发(GA_LEGACY_ROOT=仓库根)不设置, 避免
    污染共享 memory/ 与 temp/。显式设置 GA_WORKSPACE_MEMORY/TEMP 优先。"""
    if os.environ.get("GA_WORKSPACE_MEMORY") or os.environ.get("GA_WORKSPACE_TEMP"):
        return
    if str(legacy_root).rstrip("/\\") != "/ga/legacy":
        return  # loopback 开发: 不推断
    mem = legacy_root / "memory"
    temp = legacy_root / "temp"
    if mem.is_dir() and temp.is_dir():
        os.environ.setdefault("GA_WORKSPACE_MEMORY", str(mem))
        os.environ.setdefault("GA_WORKSPACE_TEMP", str(temp))


def _parse_listen(address: str) -> str:
    address = address.strip()
    if not address:
        raise SystemExit("listen address required")
    # Accept tcp:host:port, host:port, or unix:path / unix:///path
    if address.startswith("unix:"):
        return address
    if address.startswith("tcp:"):
        return address[len("tcp:") :]
    return address


def build_adapter_from_env() -> ManagedAgentAdapter:
    config_root = Path(_require_env("GA_CONFIG_ROOT"))
    legacy_root = Path(_require_env("GA_LEGACY_ROOT"))
    runtime_root = Path(_require_env("GA_RUNTIME_DIR"))
    policy_file = Path(_require_env("GA_POLICY_FILE"))

    if not config_root.is_dir():
        raise SystemExit(f"GA_CONFIG_ROOT is not a directory: {config_root}")
    if not legacy_root.is_dir():
        raise SystemExit(f"GA_LEGACY_ROOT is not a directory: {legacy_root}")
    if not (legacy_root / "agentmain.py").is_file():
        raise SystemExit(f"agentmain.py missing under GA_LEGACY_ROOT: {legacy_root}")
    _infer_workspace_roots(legacy_root)
    runtime_root.mkdir(parents=True, exist_ok=True)

    try:
        registry = CapabilityRegistry.load(policy_file)
    except Exception as exc:
        raise SystemExit(f"invalid GA_POLICY_FILE: {exc}") from exc

    # Policy must load before any legacy import (adapter imports on start_session).
    return ManagedAgentAdapter(
        config_root=config_root,
        legacy_root=legacy_root,
        runtime_root=runtime_root,
        registry=registry,
        agent_factory=None,
    )


def serve(listen: str, adapter: ManagedAgentAdapter, *, grace_seconds: float = 10.0, tls_cert_path: str = "", tls_key_path: str = "", tls_ca_path: str = "") -> None:
    from concurrent.futures import ThreadPoolExecutor

    server = grpc.server(ThreadPoolExecutor(max_workers=8))
    add_worker_servicer(server, adapter)
    if tls_cert_path and tls_key_path and tls_ca_path:
        _require_file(tls_cert_path, "GA_RUNNER_TLS_CERT")
        _require_file(tls_key_path, "GA_RUNNER_TLS_KEY")
        _require_file(tls_ca_path, "GA_RUNNER_TLS_CA")
        with open(tls_cert_path, "rb") as f:
            cert_bytes = f.read()
        with open(tls_key_path, "rb") as f:
            key_bytes = f.read()
        with open(tls_ca_path, "rb") as f:
            ca_bytes = f.read()
        credentials = grpc.ssl_server_credentials(
            ((key_bytes, cert_bytes),),
            root_certificates=ca_bytes,
            require_client_auth=True,  # 仅接受 Platform control identity(方案 §7)
        )
        bound = server.add_secure_port(listen, credentials)
        transport = "mTLS"
    else:
        bound = server.add_insecure_port(listen)
        transport = "insecure"
    if bound == 0:
        raise SystemExit(f"failed to bind listen address: {listen}")
    server.start()
    # bound is the concrete port when listen used :0
    if listen.endswith(":0") or listen.rstrip().endswith("0"):
        host = listen.rsplit(":", 1)[0]
        print(f"WORKER_LISTEN={host}:{bound}", flush=True)
    else:
        print(f"WORKER_LISTEN={listen}", flush=True)
    print(f"ga_worker listening on {listen} (bound={bound}, transport={transport})", flush=True)

    stop = threading.Event()

    def _handle_signal(signum, frame):
        stop.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(sig, _handle_signal)
        except Exception:
            pass

    try:
        while not stop.is_set():
            stop.wait(timeout=0.5)
    finally:
        # Graceful: cooperative cancel, wait grace, then stop.
        try:
            adapter.shutdown("entrypoint-shutdown")
        except Exception as exc:
            print(f"shutdown request error: {exc}", flush=True)
        stopped = server.stop(grace=grace_seconds)
        try:
            stopped.wait(grace_seconds + 1)
        except Exception:
            print("shutdown grace period elapsed with blocked calls", flush=True)


def _require_file(path: str, env_name: str) -> None:
    p = Path(path)
    if not p.is_file():
        raise SystemExit(f"{env_name} is not a file: {path}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="GenericAgent tenant Worker")
    parser.add_argument(
        "--listen",
        default=os.environ.get("GA_WORKER_LISTEN", "127.0.0.1:0"),
        help="Unix or TCP listen address (tcp:host:port, host:port, unix:/path)",
    )
    parser.add_argument(
        "--grace-seconds",
        type=float,
        default=float(os.environ.get("GA_WORKER_GRACE_SECONDS", "10")),
    )
    parser.add_argument(
        "--tls-cert",
        default=os.environ.get("GA_RUNNER_TLS_CERT", ""),
        help="Runner service certificate PEM (mTLS; 方案 §7)",
    )
    parser.add_argument(
        "--tls-key",
        default=os.environ.get("GA_RUNNER_TLS_KEY", ""),
        help="Runner service key PEM (mTLS)",
    )
    parser.add_argument(
        "--tls-ca",
        default=os.environ.get("GA_RUNNER_TLS_CA", ""),
        help="Platform CA PEM used to verify client certificates (mTLS)",
    )
    args = parser.parse_args(argv)
    listen = _parse_listen(args.listen)
    adapter = build_adapter_from_env()
    serve(
        listen,
        adapter,
        grace_seconds=args.grace_seconds,
        tls_cert_path=args.tls_cert,
        tls_key_path=args.tls_key,
        tls_ca_path=args.tls_ca,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
