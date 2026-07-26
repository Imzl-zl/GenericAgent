"""Fetch mykey.py configuration from Platform at Worker startup."""

import os
import logging
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


def fetch_mykey_config(platform_url: str, token: str) -> str:
    """从 Platform API 拉取最新的 mykey.py 配置内容。

    Args:
        platform_url: Platform 的 base URL (例如 http://127.0.0.1:8080)
        token: Worker 认证 token (X-Platform-Dev-Token)

    Returns:
        mykey.py 的内容（Python 代码字符串）

    Raises:
        RuntimeError: 如果拉取失败
    """
    try:
        import requests
    except ImportError:
        raise RuntimeError("requests library is required to fetch config from Platform")

    url = f"{platform_url.rstrip('/')}/v1/config/mykey.py"
    headers = {"X-Platform-Dev-Token": token}

    try:
        logger.info(f"Fetching mykey.py from {url}")
        resp = requests.get(url, headers=headers, timeout=10)
        resp.raise_for_status()
        content = resp.text
        logger.info(f"Successfully fetched mykey.py ({len(content)} bytes)")
        return content
    except requests.RequestException as e:
        logger.error(f"Failed to fetch mykey.py from Platform: {e}")
        raise RuntimeError(f"Failed to fetch mykey.py: {e}") from e


def write_mykey(config_root: Path, content: str) -> None:
    """将 mykey.py 内容写入配置目录。

    Args:
        config_root: GA 配置根目录
        content: mykey.py 的内容
    """
    mykey_path = config_root / "mykey.py"
    try:
        mykey_path.write_text(content, encoding="utf-8")
        logger.info(f"Wrote mykey.py to {mykey_path}")
    except Exception as e:
        logger.error(f"Failed to write mykey.py: {e}")
        raise RuntimeError(f"Failed to write mykey.py: {e}") from e


def ensure_mykey(config_root: Path, platform_url: str | None = None, token: str | None = None) -> bool:
    """确保 mykey.py 存在且是最新的。

    工作流程：
    1. 如果提供了 platform_url 和 token，从 Platform 拉取最新配置
    2. 如果拉取失败，检查本地是否有备份
    3. 如果本地也没有，抛出异常

    Args:
        config_root: GA 配置根目录
        platform_url: Platform 的 base URL（可选）
        token: Worker 认证 token（可选）

    Returns:
        True 如果成功，False 如果使用了本地备份

    Raises:
        RuntimeError: 如果无法获取配置
    """
    config_root = Path(config_root)
    mykey_path = config_root / "mykey.py"

    # 如果提供了 Platform 信息，尝试拉取最新配置
    if platform_url and token:
        try:
            content = fetch_mykey_config(platform_url, token)
            write_mykey(config_root, content)
            return True
        except Exception as e:
            logger.warning(f"Failed to fetch from Platform: {e}, checking local fallback")

    # 检查本地是否有备份
    if mykey_path.exists():
        logger.info(f"Using local mykey.py at {mykey_path}")
        return False

    # 既拉取失败又没有本地备份
    raise RuntimeError(
        "No mykey.py available: Platform fetch failed and no local fallback found. "
        "Please ensure Platform is running and PLATFORM_URL/PLATFORM_DEV_TOKEN are set correctly."
    )


def get_platform_config_from_env() -> tuple[str | None, str | None]:
    """从环境变量中获取 Platform 配置。

    Returns:
        (platform_url, token) 元组，如果未设置则为 None
    """
    platform_url = os.environ.get("PLATFORM_URL")
    token = os.environ.get("PLATFORM_DEV_TOKEN")
    return platform_url, token
