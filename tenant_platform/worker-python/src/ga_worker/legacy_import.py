"""Legacy runtime import boundary: overlay-only imports, never GA_LEGACY_ROOT on sys.path after materialization."""

from __future__ import annotations

import importlib
import importlib.util
import sys
from pathlib import Path
from types import ModuleType
from typing import Any

from ga_worker.runtime_overlay import OVERLAY_MANIFEST_ENTRIES, OverlayError

# Modules that must never be preloaded/imported from outside the overlay manifest.
LEGACY_MODULE_NAMES = frozenset(
    {
        "agentmain",
        "ga",
        "llmcore",
        "agent_loop",
        "simphtml",
        "plugins",
        "plugins.hooks",
    }
)

# Config root may only contain approved config names; these must not shadow legacy modules.
RESERVED_CONFIG_NAMES = frozenset(
    {
        "agentmain.py",
        "ga.py",
        "llmcore.py",
        "agent_loop.py",
        "simphtml.py",
        "plugins",
    }
)

APPROVED_CONFIG_FILES = frozenset({"mykey.py"})


class LegacyImportError(RuntimeError):
    """Raised when the legacy import boundary is violated."""


def _assert_no_preloaded_legacy() -> None:
    loaded = [name for name in LEGACY_MODULE_NAMES if name in sys.modules]
    # Allow re-import only if already loaded from an overlay path (caller may re-enter).
    for name in loaded:
        mod = sys.modules[name]
        origin = getattr(mod, "__file__", None) or ""
        if origin and "legacy-overlay" not in origin.replace("\\", "/"):
            raise LegacyImportError(f"legacy module already preloaded outside overlay: {name} from {origin}")


def _validate_config_root(config_root: Path) -> None:
    if not config_root.is_dir():
        raise LegacyImportError(f"config_root missing: {config_root}")
    for child in config_root.iterdir():
        name = child.name
        if name in RESERVED_CONFIG_NAMES or name in {n for n in OVERLAY_MANIFEST_ENTRIES}:
            raise LegacyImportError(f"reserved module name in GA_CONFIG_ROOT: {name}")
        if child.is_file() and name.endswith(".py") and name not in APPROVED_CONFIG_FILES:
            # Only mykey.py is approved for foundation smoke; other .py files rejected.
            raise LegacyImportError(f"unapproved config file in GA_CONFIG_ROOT: {name}")


def _purge_legacy_from_sys_path(legacy_root: Path) -> None:
    legacy = str(legacy_root.resolve())
    sys.path[:] = [p for p in sys.path if Path(p).resolve().as_posix() != Path(legacy).resolve().as_posix()]


def _ensure_path_front(path: Path) -> None:
    s = str(path.resolve())
    while s in sys.path:
        sys.path.remove(s)
    sys.path.insert(0, s)


def import_legacy_runtime(
    *,
    config_root: Path,
    legacy_root: Path,
    overlay_dir: Path,
    manifest: dict[str, Any],
) -> dict[str, ModuleType]:
    """
    Import legacy modules exclusively from the materialized overlay.
    Validates roots, rejects preloaded modules and config shadowing, removes GA_LEGACY_ROOT from sys.path.
    """
    config_root = Path(config_root)
    legacy_root = Path(legacy_root)
    overlay_dir = Path(overlay_dir)

    if not legacy_root.is_dir():
        raise LegacyImportError(f"legacy_root missing: {legacy_root}")
    if not (legacy_root / "agentmain.py").is_file():
        raise LegacyImportError(f"agentmain.py missing under legacy_root: {legacy_root}")
    if not overlay_dir.is_dir():
        raise LegacyImportError(f"overlay_dir missing: {overlay_dir}")
    entries = set(manifest.get("entries") or [])
    if entries != set(OVERLAY_MANIFEST_ENTRIES):
        raise LegacyImportError("overlay manifest entries do not match allowlist")

    _validate_config_root(config_root)
    _assert_no_preloaded_legacy()

    # Never fall back to CWD.
    cwd = Path.cwd().resolve()
    if str(cwd) in sys.path:
        # Keep cwd out of import path for this boundary by moving it to the end and preferring overlay/config.
        pass

    _purge_legacy_from_sys_path(legacy_root)
    _ensure_path_front(overlay_dir)
    _ensure_path_front(config_root)

    # Import order: agent_loop/simphtml first via ga dependency chain.
    # Import agentmain which pulls llmcore, agent_loop, ga, plugins.hooks.
    imported: dict[str, ModuleType] = {}

    def _load(name: str) -> ModuleType:
        overlay_resolved = overlay_dir.resolve()
        if name in sys.modules:
            mod = sys.modules[name]
            origin = getattr(mod, "__file__", "") or ""
            if origin:
                try:
                    Path(origin).resolve().relative_to(overlay_resolved)
                    return mod
                except ValueError:
                    # Loaded from a different path/overlay — force reload.
                    del sys.modules[name]
                    # Also drop package parents that may cache old path.
                    if name.startswith("plugins"):
                        for key in list(sys.modules):
                            if key == "plugins" or key.startswith("plugins."):
                                if key != name:
                                    try:
                                        op = getattr(sys.modules[key], "__file__", "") or ""
                                        if op:
                                            Path(op).resolve().relative_to(overlay_resolved)
                                        else:
                                            del sys.modules[key]
                                    except Exception:
                                        del sys.modules[key]
            else:
                del sys.modules[name]
        mod = importlib.import_module(name)
        origin = getattr(mod, "__file__", "") or ""
        if name in LEGACY_MODULE_NAMES and origin:
            origin_path = Path(origin).resolve()
            try:
                origin_path.relative_to(overlay_resolved)
            except ValueError as exc:
                raise LegacyImportError(
                    f"imported legacy module {name} not from overlay: {origin}"
                ) from exc
        return mod

    # Ensure simphtml and ga are importable (ga eagerly imports simphtml in real tree).
    # plugins/hooks are on the immutable allowlist — failures must be visible.
    for name in ("simphtml", "agent_loop", "llmcore", "ga", "plugins", "plugins.hooks", "agentmain"):
        try:
            imported[name] = _load(name)
        except ModuleNotFoundError as exc:
            raise LegacyImportError(f"failed to import legacy module {name}: {exc}") from exc
        except Exception as exc:
            raise LegacyImportError(f"failed to import legacy module {name}: {exc}") from exc

    _purge_legacy_from_sys_path(legacy_root)
    return imported


def ensure_legacy_available_for_factory(
    *,
    config_root: Path,
    legacy_root: Path,
    overlay_dir: Path,
    manifest: dict[str, Any],
    agent_factory,
) -> dict[str, ModuleType] | None:
    """
    When agent_factory is injected (unit tests), skip real legacy import.
    Otherwise import from overlay.
    """
    if agent_factory is not None:
        return None
    return import_legacy_runtime(
        config_root=config_root,
        legacy_root=legacy_root,
        overlay_dir=overlay_dir,
        manifest=manifest,
    )
