"""Media download/decrypt helpers for WeChat iLink Bot.

Separated from wxbot_client.py to keep the protocol client focused on
send/receive. Both GA Core (wechatapp.py) and the platform Bot Poller
import this for inbound media decryption.
"""

import os, base64, hashlib, uuid
from urllib.parse import quote
import requests
from Crypto.Cipher import AES

CDN_BASE = 'https://novac2c.cdn.weixin.qq.com/c2c'
UA = f'openclaw-weixin/2.1.10'

_MEDIA_KEYS = {'image_item': '.jpg', 'video_item': '.mp4', 'file_item': '', 'voice_item': '.silk'}

# WeChat's own per-file limit is ~100MB; anything larger is not a legitimate
# iLink media payload. Bounds both memory usage and disk consumption.
MAX_MEDIA_BYTES = 100 * 1024 * 1024
_DL_CHUNK = 64 * 1024


def download_media(items, dest_dir=None, with_names=False):
    """Download & decrypt all media items from an inbound message.

    Args:
        items: the item_list from an iLink message.
        dest_dir: where to write decrypted files. Defaults to ../temp.
        with_names: True 时返回 [(path, original_file_name), ...] 元组列表——
            落盘名含内容 hash 前缀, Poller 需要原始 file_name 做用户可见名
            (Round8 审查: 不按位置索引 item_list, 下载失败的项会使索引错位)。

    Returns:
        List of local file paths for successfully decrypted media.
    """
    if dest_dir is None:
        dest_dir = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), 'temp')
    os.makedirs(dest_dir, exist_ok=True)
    paths = []
    for item in items:
        for key, ext in _MEDIA_KEYS.items():
            sub = item.get(key)
            if not sub:
                continue
            path = _decrypt_one(sub, ext, dest_dir)
            if path:
                if with_names:
                    paths.append((path, sub.get('file_name') or os.path.basename(path)))
                else:
                    paths.append(path)
            break  # one media per item
    return paths


def _safe_filename(name, ext):
    """Strip any directory components from a sender-controlled file name.

    file_name comes straight from the message payload; without basename a
    crafted name like "..\\..\\evil.py" would escape dest_dir (path traversal).
    Falls back to a random name when the sanitized result is empty.
    """
    base = os.path.basename((name or '').replace('\\', '/'))
    if base in ('', '.', '..'):
        base = f'{uuid.uuid4().hex[:8]}{ext or ".bin"}'
    return base


def _download_bounded(url):
    """Stream-download url, aborting past MAX_MEDIA_BYTES. Returns bytes or None."""
    with requests.get(url, headers={'User-Agent': UA}, timeout=60, stream=True) as resp:
        resp.raise_for_status()
        clen = resp.headers.get('Content-Length')
        if clen and int(clen) > MAX_MEDIA_BYTES:
            raise ValueError(f'media too large: Content-Length={clen}')
        chunks, total = [], 0
        for chunk in resp.iter_content(chunk_size=_DL_CHUNK):
            total += len(chunk)
            if total > MAX_MEDIA_BYTES:
                raise ValueError(f'media exceeded {MAX_MEDIA_BYTES} bytes')
            chunks.append(chunk)
        return b''.join(chunks)


def _pkcs7_unpad(pt):
    """Validated PKCS#7 unpad. Raises ValueError on malformed padding.

    The old `pt[:-pt[-1]]` crashed with IndexError on empty input and silently
    corrupted data when the last byte was 0 or > 16.
    """
    if not pt:
        raise ValueError('empty plaintext')
    pad = pt[-1]
    if pad < 1 or pad > 16 or pad > len(pt):
        raise ValueError(f'invalid PKCS#7 padding: {pad}')
    return pt[:-pad]


def _decrypt_one(sub, ext, dest_dir):
    """Download and decrypt a single media item. Returns path or None."""
    import sys
    eq = (sub.get('media') or {}).get('encrypt_query_param')
    if not eq:
        return None
    ak = (sub.get('media') or {}).get('aes_key', '') or sub.get('aeskey', '')
    if not ak:
        return None
    try:
        aes_key = (bytes.fromhex(base64.b64decode(ak).decode())
                   if sub.get('media', {}).get('aes_key') else bytes.fromhex(ak))
        ct = _download_bounded(f'{CDN_BASE}/download?encrypted_query_param={quote(eq)}')
        pt = _pkcs7_unpad(AES.new(aes_key, AES.MODE_ECB).decrypt(ct))

        # Verify protocol-provided MD5 when present (FileItem carries md5).
        expected_md5 = sub.get('md5') or sub.get('file_md5') or ''
        if expected_md5 and hashlib.md5(pt).hexdigest() != expected_md5.lower():
            print(f'[WX] media md5 mismatch, dropped ({ext})', file=sys.__stdout__)
            return None

        fname = _safe_filename(sub.get('file_name'), ext)
        # Round8 审查: 落盘名加内容 hash 前缀——同一批次/coalescing 窗口内
        # 两个同名不同内容文件不再互相覆盖; 同内容重试下载得到相同文件名
        # (os.replace 原子覆盖), 不产生重复残留。用户可见名由 Poller 从
        # 原始 file_name 恢复(media_items), 不受前缀影响。
        digest = hashlib.md5(pt).hexdigest()[:10]
        p = os.path.join(dest_dir, f'{digest}_{fname}')
        # Atomic write: never leave a half-written file for the worker to read.
        tmp = p + f'.{uuid.uuid4().hex[:8]}.tmp'
        try:
            with open(tmp, 'wb') as f:
                f.write(pt)
            os.replace(tmp, p)
        except Exception:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise
        print(f'[WX] media saved: {fname} ({len(pt)} bytes)', file=sys.__stdout__)
        return p
    except Exception as e:
        print(f'[WX] media dl err ({ext}): {e}', file=sys.__stdout__)
        return None
