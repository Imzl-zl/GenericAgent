from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
from pathlib import Path

import pytest


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
RELEASE_SCRIPT = REPOSITORY_ROOT / "infra" / "compose" / "release_images.py"
GOOD_DIGEST = "sha256:" + "a" * 64


def load_release_module():
    spec = importlib.util.spec_from_file_location("release_images_test_module", RELEASE_SCRIPT)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def test_release_images_dry_run_plans_all_published_images() -> None:
    result = subprocess.run(
        [
            sys.executable,
            str(RELEASE_SCRIPT),
            "--registry",
            "registry.example/acme",
            "--tag",
            "reviewed-sha",
            "--platform",
            "linux/amd64",
            "--dry-run",
        ],
        cwd=REPOSITORY_ROOT,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert "docker buildx build --platform linux/amd64 --pull --push" in result.stdout
    assert "tenant_platform/infra/compose/platform.Dockerfile" in result.stdout
    assert "tenant_platform/infra/compose/bot-poller.Dockerfile" in result.stdout
    assert "tenant_platform/infra/compose/web.Dockerfile" in result.stdout
    assert "tenant_platform/document-image/Dockerfile" in result.stdout
    for image in (
        "genericagent-platform",
        "genericagent-bot-poller",
        "genericagent-web",
        "genericagent-document-tool",
    ):
        assert f"registry.example/acme/{image}:reviewed-sha" in result.stdout


def test_release_images_uses_build_metadata_instead_of_mutable_tag_lookup(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    module = load_release_module()

    def write_build_metadata(command: list[str]) -> None:
        if "--metadata-file" in command:
            metadata_path = Path(command[command.index("--metadata-file") + 1])
            metadata_path.write_text(json.dumps({"containerimage.digest": GOOD_DIGEST}), encoding="utf-8")

    def reject_tag_lookup(command: list[str]) -> str:
        raise AssertionError(f"release script must not resolve a mutable tag: {command}")

    monkeypatch.setattr(module, "ensure_reviewed_checkout", lambda: None)
    monkeypatch.setattr(module, "run", write_build_metadata)
    monkeypatch.setattr(module, "run_capture", reject_tag_lookup)
    monkeypatch.setattr(
        sys,
        "argv",
        [
            str(RELEASE_SCRIPT),
            "--registry",
            "registry.example/acme",
            "--tag",
            "reviewed-sha",
        ],
    )

    assert module.main() == 0
    assert GOOD_DIGEST in capsys.readouterr().out
