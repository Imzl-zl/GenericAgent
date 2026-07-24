"""Immutable capability policy registry loaded once at process start."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any


SCHEMA_VERSION = "genericagent.capability-policy.v1"


@dataclass(frozen=True)
class ToolPolicy:
    version: str
    allowed_tools: frozenset[str]


class CapabilityRegistry:
    """Deny-by-default tool policy registry. No built-in fallback."""

    def __init__(self, digest: str, capabilities: dict[str, dict[str, ToolPolicy]]):
        self.digest = digest
        self._capabilities = capabilities

    @classmethod
    def load(cls, path: Path) -> "CapabilityRegistry":
        path = Path(path)
        if not path.is_file():
            raise ValueError(f"policy file not found: {path}")
        raw = path.read_bytes()
        digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        try:
            data = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ValueError(f"invalid policy JSON: {exc}") from exc
        if not isinstance(data, dict):
            raise ValueError("policy root must be an object")
        if data.get("schema_version") != SCHEMA_VERSION:
            raise ValueError(
                f"unsupported schema_version {data.get('schema_version')!r}; "
                f"expected {SCHEMA_VERSION!r}"
            )
        caps_raw = data.get("capabilities")
        if not isinstance(caps_raw, dict) or not caps_raw:
            raise ValueError("capabilities must be a non-empty object")

        capabilities: dict[str, dict[str, ToolPolicy]] = {}
        seen_policy_names: set[str] = set()
        for cap_name, cap_body in caps_raw.items():
            if not isinstance(cap_name, str) or not cap_name.strip():
                raise ValueError("capability name must be a non-empty string")
            if not isinstance(cap_body, dict):
                raise ValueError(f"capability {cap_name!r} must be an object")
            policies_raw = cap_body.get("tool_policies")
            if not isinstance(policies_raw, dict) or not policies_raw:
                raise ValueError(f"capability {cap_name!r} needs non-empty tool_policies")
            pol_map: dict[str, ToolPolicy] = {}
            for pol_name, pol_body in policies_raw.items():
                if not isinstance(pol_name, str) or not pol_name.strip():
                    raise ValueError("tool_policy version must be a non-empty string")
                if pol_name in seen_policy_names:
                    raise ValueError(f"duplicate tool_policy version {pol_name!r}")
                seen_policy_names.add(pol_name)
                if not isinstance(pol_body, dict):
                    raise ValueError(f"tool_policy {pol_name!r} must be an object")
                allowed = pol_body.get("allowed_tools")
                if not isinstance(allowed, list) or not allowed:
                    raise ValueError(f"tool_policy {pol_name!r} allowed_tools must be non-empty")
                names: list[str] = []
                for t in allowed:
                    if not isinstance(t, str) or not t.strip():
                        raise ValueError(f"tool_policy {pol_name!r} has empty tool name")
                    if t in names:
                        raise ValueError(f"tool_policy {pol_name!r} has duplicate tool {t!r}")
                    names.append(t)
                pol_map[pol_name] = ToolPolicy(version=pol_name, allowed_tools=frozenset(names))
            capabilities[cap_name] = pol_map
        return cls(digest=digest, capabilities=capabilities)

    def resolve(self, capability_version: str, tool_policy_version: str) -> ToolPolicy:
        if capability_version not in self._capabilities:
            raise ValueError(f"unknown capability_version: {capability_version!r}")
        pol_map = self._capabilities[capability_version]
        if tool_policy_version not in pol_map:
            # Cross-capability or unknown: scan others for clearer error.
            for other_cap, other_map in self._capabilities.items():
                if other_cap != capability_version and tool_policy_version in other_map:
                    raise ValueError(
                        f"tool_policy_version {tool_policy_version!r} belongs to "
                        f"capability {other_cap!r}, not {capability_version!r}"
                    )
            raise ValueError(f"unknown tool_policy_version: {tool_policy_version!r}")
        return pol_map[tool_policy_version]

    def capability_versions(self) -> frozenset[str]:
        return frozenset(self._capabilities)
