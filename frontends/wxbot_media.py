"""Media download/decrypt helpers for WeChat iLink Bot.

Separated from wxbot_client.py to keep the protocol client focused on
send/receive. Both GA Core (wechatapp.py) and the platform Bot Poller
import this for inbound media decryption.
"""

import os, base64, uuid
from urllib.parse import quote
import requests
from Crypto.Cipher import AES

CDN_BASE = 'https://novac2c.cdn.weixin.qq.com/c2c'
UA = f'openclaw-weixin/2.1.10'

_MEDIA_KEYS = {'image_item': '.jpg', 'video_item': '.mp4', 'file_item': '', 'voice_item': '.silk'}


def download_media(items, dest_dir=None):
    """Download & decrypt all media items from an inbound message.

    Args:
        items: the item_list from an iLink message.
        dest_dir: where to write decrypted files. Defaults to ../temp.

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
                paths.append(path)
            break  # one media per item
    return paths


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
        ct = requests.get(f'{CDN_BASE}/download?encrypted_query_param={quote(eq)}',
                          headers={'User-Agent': UA}, timeout=60).content
        pt = AES.new(aes_key, AES.MODE_ECB).decrypt(ct)
        pt = pt[:-pt[-1]]
        fname = sub.get('file_name') or f'{uuid.uuid4().hex[:8]}{ext or ".bin"}'
        p = os.path.join(dest_dir, fname)
        with open(p, 'wb') as f:
            f.write(pt)
        print(f'[WX] media saved: {fname}', file=sys.__stdout__)
        return p
    except Exception as e:
        print(f'[WX] media dl err ({ext}): {e}', file=sys.__stdout__)
        return None
