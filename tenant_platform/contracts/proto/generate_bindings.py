#!/usr/bin/env python3
"""Generate checked-in Go and Python Worker protobuf bindings.

只生成 worker.proto。LLM Proxy 契约以 HTTP 实现为准
(backend-go/internal/infrastructure/llmproxy, 路由 /v1/chat/completions 等),
不生成 gRPC proxy 绑定。"""

from __future__ import annotations

import shutil
import subprocess
import sys
from pathlib import Path


def require_executable(name: str) -> str:
    executable = shutil.which(name)
    if executable is None:
        raise SystemExit(f"required executable not found: {name}")
    return executable


def run(command: list[str], cwd: Path) -> None:
    subprocess.run(command, cwd=cwd, check=True)


def main() -> None:
    repo = Path(__file__).resolve().parents[3]
    proto_root = repo / "tenant_platform" / "contracts" / "proto"
    proto = proto_root / "genericagent" / "worker" / "v1" / "worker.proto"
    go_root = repo / "tenant_platform" / "backend-go"
    python_root = repo / "tenant_platform" / "worker-python" / "src"
    module = "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go"

    protoc = require_executable("protoc")
    require_executable("protoc-gen-go")
    require_executable("protoc-gen-go-grpc")
    run(
        [
            protoc,
            f"-I{proto_root}",
            f"--go_out={go_root}",
            f"--go_opt=module={module}",
            f"--go-grpc_out={go_root}",
            f"--go-grpc_opt=module={module}",
            str(proto),
        ],
        repo,
    )
    run(
        [
            sys.executable,
            "-m",
            "grpc_tools.protoc",
            f"-I{proto_root}",
            f"--python_out={python_root}",
            f"--grpc_python_out={python_root}",
            str(proto),
        ],
        repo,
    )


if __name__ == "__main__":
    main()
