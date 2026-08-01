"""SOP workspace 安装工具测试(方案 §5.2)。"""

from __future__ import annotations

import json

import pytest

from ga_worker.sop_tool import (
    SopToolError,
    install_sop_to_workspace,
    load_runtime_sophub_proxy,
    sops_dir,
)


def _proxy() -> object:
    from ga_worker.sop_tool import RuntimeSophubProxy

    return RuntimeSophubProxy(base_url="http://127.0.0.1:9999", capability_token="tok")


def test_install_writes_to_workspace_sops(tmp_path):
    ws_memory = tmp_path / "memory"
    ws_memory.mkdir()
    path = install_sop_to_workspace(_proxy(), ws_memory, "sop-abc", "# SOP\ncontent\n")
    assert path == ws_memory / "sops" / "sop-abc.md"
    assert path.read_text(encoding="utf-8") == "# SOP\ncontent\n"
    assert (ws_memory / "sops" / "sop-abc.md.sophub-installed").is_file()


def test_install_rejects_path_traversal_id(tmp_path):
    ws_memory = tmp_path / "memory"
    ws_memory.mkdir()
    with pytest.raises(SopToolError):
        install_sop_to_workspace(_proxy(), ws_memory, "../../etc/passwd", "x")


def test_install_rejects_oversize_content(tmp_path):
    ws_memory = tmp_path / "memory"
    ws_memory.mkdir()
    with pytest.raises(SopToolError):
        install_sop_to_workspace(_proxy(), ws_memory, "big", "x" * (64 * 1024 + 1))


def test_install_does_not_overwrite_modified_sop(tmp_path):
    ws_memory = tmp_path / "memory"
    ws_memory.mkdir()
    path = install_sop_to_workspace(_proxy(), ws_memory, "sop-mod", "v1")
    # 用户修改本地 SOP。
    path.write_text("user edited", encoding="utf-8")
    with pytest.raises(SopToolError) as ei:
        install_sop_to_workspace(_proxy(), ws_memory, "sop-mod", "v2")
    assert ei.value.code == "SOP_LOCAL_MODIFIED"
    assert path.read_text(encoding="utf-8") == "user edited"


def test_reinstall_same_content_updates(tmp_path):
    ws_memory = tmp_path / "memory"
    ws_memory.mkdir()
    install_sop_to_workspace(_proxy(), ws_memory, "sop-upd", "v1")
    path = install_sop_to_workspace(_proxy(), ws_memory, "sop-upd", "v1")
    assert path.read_text(encoding="utf-8") == "v1"


def test_load_runtime_sophub_proxy_from_config(tmp_path):
    import hashlib

    config = tmp_path / "config"
    config.mkdir()
    # checksum 是对占位符替换后内容的 sha256(与 credential_config 校验一致)。
    placeholder = "0000000000000000000000000000000000000000000000000000000000000000"
    doc = {
        "_platform_runtime": {
            "credential_generation": 1,
            "config_checksum": placeholder,
            "routing_snapshot_id": "snap-1",
        },
        "_platform_sophub": {"base_url": "http://p:1", "capability_token": "t"},
    }
    encoded = json.dumps(doc).encode("utf-8")
    checksum = hashlib.sha256(encoded.replace(placeholder.encode(), placeholder.encode(), 1)).hexdigest()
    doc["_platform_runtime"]["config_checksum"] = checksum
    (config / "mykey.runtime.json").write_text(json.dumps(doc), encoding="utf-8")
    proxy = load_runtime_sophub_proxy(config)
    assert proxy is not None
    assert proxy.base_url == "http://p:1"
    assert proxy.capability_token == "t"


def test_sops_dir_creates(tmp_path):
    ws_memory = tmp_path / "memory"
    ws_memory.mkdir()
    d = sops_dir(ws_memory)
    assert d.is_dir()
    assert d.name == "sops"
