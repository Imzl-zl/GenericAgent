#!/usr/bin/env python3
"""Build, push, and resolve the four Linux images for a reviewed release."""

from __future__ import annotations

import argparse
import json
import re
import shlex
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path


REPOSITORY_ROOT = Path(__file__).resolve().parents[3]
DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}\Z")
TAG_RE = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\Z")


@dataclass(frozen=True)
class ImageSpec:
    name: str
    dockerfile: str
    compose_variable: str


IMAGE_SPECS = (
    ImageSpec("genericagent-platform", "tenant_platform/infra/compose/platform.Dockerfile", "GA_PLATFORM_IMAGE"),
    ImageSpec("genericagent-bot-poller", "tenant_platform/infra/compose/bot-poller.Dockerfile", "GA_BOT_POLLER_IMAGE"),
    ImageSpec("genericagent-web", "tenant_platform/infra/compose/web.Dockerfile", "GA_WEB_IMAGE"),
    ImageSpec("genericagent-document-tool", "tenant_platform/document-image/Dockerfile", "DOCUMENT_MANAGER_IMAGE"),
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Build and push GenericAgent Linux images, then print immutable digest references."
    )
    parser.add_argument("--registry", required=True, help="OCI registry/repository prefix, for example registry.example/acme")
    parser.add_argument("--tag", help="Image tag used only while publishing; defaults to the checked-out Git revision")
    parser.add_argument("--platform", default="linux/amd64", help="Buildx target platform (default: linux/amd64)")
    parser.add_argument("--dry-run", action="store_true", help="Print planned Docker commands without executing them")
    return parser.parse_args()


def normalize_registry(raw: str) -> str:
    registry = raw.strip().strip("/")
    if not registry or "://" in registry or "@" in registry or any(char.isspace() for char in registry):
        raise ValueError("--registry must be an OCI registry/repository prefix without a scheme or digest")
    return registry


def resolve_tag(explicit_tag: str | None) -> str:
    if explicit_tag:
        tag = explicit_tag.strip()
    else:
        tag = run_capture(["git", "rev-parse", "--short=12", "HEAD"]).strip()
    if not TAG_RE.fullmatch(tag):
        raise ValueError("--tag must be a Docker tag")
    return tag


def build_command(platform: str, dockerfile: str, image: str, metadata_file: str) -> list[str]:
    return [
        "docker",
        "buildx",
        "build",
        "--platform",
        platform,
        "--pull",
        "--push",
        "--metadata-file",
        metadata_file,
        "--file",
        dockerfile,
        "--tag",
        image,
        ".",
    ]


def read_build_digest(metadata_file: Path) -> str:
    try:
        metadata = json.loads(metadata_file.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise RuntimeError(f"cannot read buildx metadata: {metadata_file}") from exc
    digest = metadata.get("containerimage.digest")
    if not isinstance(digest, str):
        descriptor = metadata.get("containerimage.descriptor")
        if isinstance(descriptor, dict):
            digest = descriptor.get("digest")
    if not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest):
        raise RuntimeError(f"buildx metadata has no valid manifest digest: {metadata_file}")
    return digest


def run_capture(command: list[str]) -> str:
    try:
        result = subprocess.run(
            command,
            cwd=REPOSITORY_ROOT,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except FileNotFoundError as exc:
        raise RuntimeError(f"required command is unavailable: {command[0]}") from exc
    if result.returncode != 0:
        raise RuntimeError(f"command failed: {shlex.join(command)}\n{result.stderr.strip()}")
    return result.stdout


def run(command: list[str]) -> None:
    try:
        result = subprocess.run(command, cwd=REPOSITORY_ROOT, check=False)
    except FileNotFoundError as exc:
        raise RuntimeError(f"required command is unavailable: {command[0]}") from exc
    if result.returncode != 0:
        raise RuntimeError(f"command failed: {shlex.join(command)}")


def ensure_reviewed_checkout() -> None:
    changes = run_capture(["git", "status", "--porcelain", "--untracked-files=all"])
    if changes.strip():
        raise RuntimeError("refusing to publish from a dirty checkout; commit or remove local changes first")


def print_release_references(digests: dict[str, str]) -> None:
    print("\n# Compose .env image references")
    for spec in IMAGE_SPECS[:3]:
        print(f"{spec.compose_variable}={digests[spec.name]}")
    print("\n# /etc/ga/document-manager.env image reference")
    document = IMAGE_SPECS[3]
    print(f"{document.compose_variable}={digests[document.name]}")


def main() -> int:
    args = parse_args()
    try:
        registry = normalize_registry(args.registry)
        tag = resolve_tag(args.tag)
        if not args.platform.startswith("linux/"):
            raise ValueError("--platform must target Linux for the hardened deployment profile")

        planned: list[tuple[ImageSpec, str]] = []
        for spec in IMAGE_SPECS:
            dockerfile = REPOSITORY_ROOT / spec.dockerfile
            if not dockerfile.is_file():
                raise RuntimeError(f"Dockerfile is missing: {spec.dockerfile}")
            image = f"{registry}/{spec.name}:{tag}"
            planned.append((spec, image))

        if args.dry_run:
            print("Dry run: no Docker commands are executed.")
            for spec, image in planned:
                metadata_file = f"<{spec.name}.metadata.json>"
                print(shlex.join(build_command(args.platform, spec.dockerfile, image, metadata_file)))
            print("Buildx metadata files capture each pushed manifest digest; mutable tags are never resolved afterward.")
            return 0

        ensure_reviewed_checkout()
        digests: dict[str, str] = {}
        with tempfile.TemporaryDirectory(prefix="genericagent-release-") as temporary_directory:
            metadata_root = Path(temporary_directory)
            for spec, image in planned:
                metadata_file = metadata_root / f"{spec.name}.json"
                run(build_command(args.platform, spec.dockerfile, image, str(metadata_file)))
                digest = read_build_digest(metadata_file)
                digests[spec.name] = f"{registry}/{spec.name}@{digest}"
        print_release_references(digests)
        return 0
    except (RuntimeError, ValueError) as exc:
        print(f"release-images: error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
