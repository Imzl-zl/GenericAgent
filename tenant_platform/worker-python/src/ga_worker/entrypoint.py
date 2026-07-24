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


def serve(listen: str, adapter: ManagedAgentAdapter, *, grace_seconds: float = 10.0) -> None:
    from concurrent.futures import ThreadPoolExecutor

    server = grpc.server(ThreadPoolExecutor(max_workers=8))
    add_worker_servicer(server, adapter)
    bound = server.add_insecure_port(listen)
    if bound == 0:
        raise SystemExit(f"failed to bind listen address: {listen}")
    server.start()
    # bound is the concrete port when listen used :0
    if listen.endswith(":0") or listen.rstrip().endswith("0"):
        host = listen.rsplit(":", 1)[0]
        print(f"WORKER_LISTEN={host}:{bound}", flush=True)
    else:
        print(f"WORKER_LISTEN={listen}", flush=True)
    print(f"ga_worker listening on {listen} (bound={bound})", flush=True)

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
    args = parser.parse_args(argv)
    listen = _parse_listen(args.listen)
    adapter = build_adapter_from_env()
    serve(listen, adapter, grace_seconds=args.grace_seconds)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
