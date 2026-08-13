"""Bot Poller 通用入站媒体下载器(IM_MEDIA_ARCHITECTURE §3.1)。

渠道协议差异只有两种模式——URL 直下(微信/QQ)与 API+token(飞书/钉钉/企微);
本模块收敛公共部分:
  * URL 直下(https + host 白名单 + Content-Length 预检 + 流式累计上限 +
    原子落盘)——QQ 附件(URL 带 rkey 时效, 事件到达即下载, 审查 B2)复用
    与微信同一套"下载 → 落盘 → 元数据"语义;
  * 字节落盘(save_bytes_bounded)——飞书/钉钉/企微 API 取回二进制后复用;
  * 图片魔数嗅探——飞书 image 消息无扩展名/类型信息, 落盘名必须带正确
    扩展名, 否则 GA 注入层(media_content_blocks 按扩展名判断)会跳过;
  * 媒体项元数据构造(build_media_item)——与 WeChatAdapter._collect_media_items
    同构(storage_path 相对 media_root, 前斜杠, 跨 NFS/S3 挂载可移植);
  * 扩展名 → MIME 映射(guess_content_type, 原 poller_server._EXT_CONTENT_TYPES
    迁移至此, 供全部渠道共用)。

安全边界(审查 S1):
  * 仅 https; host 白名单默认空 = 拒绝全部(fail-closed), 调用方显式放行
    渠道平台域名(QQ=multimedia.nt.qq.com.cn, 企微图床=wework.qpic.cn);
  * 大小上限 MAX_MEDIA_BYTES=100MB, 对齐微信 wxbot_media 与 Go
    session_files.maxInboundMediaBytes(纵深防御);
  * 落盘名 = 内容 hash 前缀 + 清洗后的原始文件名(safe_filename 防路径
    穿越, 微信 Round8 同款), 原子写(临时文件 + os.replace);
  * 媒体字节 = 用户隐私数据: 落盘于 media_root/<bot_uuid>/, 留存策略由
    部署层负责(审查 I4, 待定 90d + 容量上限清理)。

调用方(adapter)职责: 事件 → 提取媒体引用(URL/file_key/downloadCode/
media_id) → 取字节(URL 直下或 API+token) → save_bytes_bounded 落盘 →
build_media_item 构造元数据 → webhook_body(media_paths, media_items)。
"""

import hashlib
import os
import uuid
from urllib.parse import urlparse

import requests

# 对齐微信 wxbot_media.MAX_MEDIA_BYTES 与 Go ImportInbound maxInboundMediaBytes。
MAX_MEDIA_BYTES = 100 * 1024 * 1024
_DL_CHUNK = 64 * 1024

# QQ 官方事件 attachments[].url 的下载域名(bot.q.qq.com wiki: C2C/GROUP_AT
# 事件 MessageAttachment.url 直带下载链接, 含 rkey 时效参数)。
QQ_MEDIA_HOSTS = ('multimedia.nt.qq.com.cn',)
# 企微智能机器人回调携带的媒体直链域名(图片等, 若无直链走 media_id 下载)。
WECOM_MEDIA_HOSTS = ('wework.qpic.cn', 'wwcdn.weixin.qq.com')

# 扩展名 → MIME(原 poller_server._EXT_CONTENT_TYPES, 迁移至此统一)。
EXT_CONTENT_TYPES = {
    '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.png': 'image/png',
    '.gif': 'image/gif', '.bmp': 'image/bmp', '.webp': 'image/webp',
    '.mp4': 'video/mp4', '.mov': 'video/quicktime', '.avi': 'video/x-msvideo',
    '.pdf': 'application/pdf',
    '.doc': 'application/msword',
    '.docx': 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    '.xls': 'application/vnd.ms-excel',
    '.xlsx': 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
    '.ppt': 'application/vnd.ms-powerpoint',
    '.pptx': 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    '.zip': 'application/zip', '.rar': 'application/x-rar-compressed',
    '.7z': 'application/x-7z-compressed', '.tar': 'application/x-tar',
    '.gz': 'application/gzip',
    '.silk': 'audio/silk', '.mp3': 'audio/mpeg', '.wav': 'audio/wav',
    '.m4a': 'audio/mp4', '.aac': 'audio/aac',
    '.txt': 'text/plain', '.md': 'text/markdown',
    '.csv': 'text/csv', '.json': 'application/json',
}


def guess_content_type(file_name):
    """按扩展名推断 MIME(默认 octet-stream)。"""
    ext = os.path.splitext(file_name)[1].lower()
    return EXT_CONTENT_TYPES.get(ext, 'application/octet-stream')


def safe_filename(name, ext=''):
    """清洗发送者可控文件名(防路径穿越)。与 wxbot_media._safe_filename 同策略。

    只保留 basename(剥离目录成分), 空/危险名回退随机名; ext 非空且
    basename 无该扩展名时补上(飞书 image 消息无文件名, 靠魔数嗅探的
    扩展名落盘, 否则 GA 注入层按扩展名判断会跳过)。
    """
    base = os.path.basename((name or '').replace('\\', '/'))
    if base in ('', '.', '..'):
        base = f'{uuid.uuid4().hex[:8]}{ext or ".bin"}'
    elif ext and not base.lower().endswith(ext.lower()):
        base = f'{base}{ext}'
    return base


def sniff_image_ext(data):
    """图片魔数嗅探: JPEG/PNG/GIF/WebP/BMP → 扩展名; 未知返回 ''。

    飞书 image 资源(只给 image_key)与部分渠道无类型信息, 落盘必须带
    正确扩展名(GA media_content_blocks 按扩展名过滤图片)。"""
    if data[:3] == b'\xff\xd8\xff':
        return '.jpg'
    if data[:8] == b'\x89PNG\r\n\x1a\n':
        return '.png'
    if data[:6] in (b'GIF87a', b'GIF89a'):
        return '.gif'
    if data[:4] == b'RIFF' and data[8:12] == b'WEBP':
        return '.webp'
    if data[:2] == b'BM':
        return '.bmp'
    return ''


def _validate_url(url, allowed_hosts):
    """仅 https + host 白名单(默认空 = 拒绝全部, fail-closed)。"""
    parsed = urlparse(url)
    if parsed.scheme != 'https':
        raise ValueError(f'insecure media url scheme: {parsed.scheme!r}')
    host = (parsed.hostname or '').lower()
    if not host:
        raise ValueError('media url has no host')
    if allowed_hosts and host not in allowed_hosts:
        raise ValueError(f'media url host {host!r} not allowed')
    return parsed


def _resolve_ext(data, file_name, ext):
    if ext:
        return ext
    if file_name:
        guessed = os.path.splitext(file_name)[1].lower()
        if guessed:
            return guessed
    return sniff_image_ext(data)


def save_bytes_bounded(data, dest_dir, *, file_name='', ext='', max_bytes=MAX_MEDIA_BYTES):
    """把渠道 API 取回的字节落盘(飞书/钉钉/企微): 大小上限 + hash 前缀 + 原子写。

    返回落盘绝对路径(供 media_paths)。同内容重试(相同 hash 前缀)直接复用
    已有落盘——微信 Round8 同款语义, 不产生重复残留。
    """
    if len(data) > max_bytes:
        raise ValueError(f'media exceeded {max_bytes} bytes')
    os.makedirs(dest_dir, exist_ok=True)
    ext = _resolve_ext(data, file_name, ext)
    safe = safe_filename(file_name, ext)
    digest = hashlib.md5(data).hexdigest()[:10]
    path = os.path.join(dest_dir, f'{digest}_{safe}')
    if os.path.exists(path):
        return path
    tmp = path + f'.{uuid.uuid4().hex[:8]}.tmp'
    try:
        with open(tmp, 'wb') as f:
            f.write(data)
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    return path


def download_url_bounded(url, dest_dir, *, file_name='', ext='', allowed_hosts=None,
                         max_bytes=MAX_MEDIA_BYTES, timeout=60):
    """URL 直下(QQ 等): https + host 白名单 + 大小上限 + 流式落盘 + 原子替换。

    与 save_bytes_bounded 同语义: 落盘名 = 内容 hash 前缀 + 清洗名, 同内容
    重试复用已有落盘(不产生重复残留)。流式写入临时文件, 内存峰值 = 缓冲块
    (2026-08-14 审查 I4: 原实现先整读再落盘, 100MB 媒体 2x 内存峰值,
    poller 512m 限下并发大文件有 OOM 风险)。失败抛异常(调用方按
    "丢弃媒体保留文本"降级)。Content-Length 预检 + 流式累计上限
    (与 wxbot_media._download_bounded 同构)。
    """
    _validate_url(url, allowed_hosts)
    os.makedirs(dest_dir, exist_ok=True)
    headers = {'User-Agent': 'GenericAgent-bot-poller'}
    tmp_path = None
    try:
        with requests.get(url, headers=headers, timeout=timeout, stream=True) as resp:
            resp.raise_for_status()
            clen = resp.headers.get('Content-Length')
            if clen and int(clen) > max_bytes:
                raise ValueError(f'media too large: Content-Length={clen}')
            tmp_path = os.path.join(dest_dir, f'.dl-{uuid.uuid4().hex[:8]}.tmp')
            digest = hashlib.md5()
            total = 0
            head = b''  # 前 16 字节供魔数嗅探(落盘名在流结束后才确定)
            with open(tmp_path, 'wb') as f:
                for chunk in resp.iter_content(chunk_size=_DL_CHUNK):
                    total += len(chunk)
                    if total > max_bytes:
                        raise ValueError(f'media exceeded {max_bytes} bytes')
                    digest.update(chunk)
                    if len(head) < 16:
                        head += chunk[:16 - len(head)]
                    f.write(chunk)
        ext = _resolve_ext(head, file_name, ext)
        safe = safe_filename(file_name, ext)
        path = os.path.join(dest_dir, f'{digest.hexdigest()[:10]}_{safe}')
        if os.path.exists(path):
            os.unlink(tmp_path)  # 同内容复用(与 save_bytes_bounded 一致)
            tmp_path = None
            return path
        os.replace(tmp_path, path)
        tmp_path = None
        return path
    finally:
        if tmp_path is not None:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass


def build_media_item(path, media_root, file_name='', content_type=None):
    """构造 webhook media_items 条目(与 WeChatAdapter._collect_media_items 同构)。

    storage_path 相对 media_root(前斜杠, 跨平台/挂载可移植); content_type
    缺省时按落盘文件名扩展名推断(魔数嗅探的扩展名已进落盘名, 推断准确)。
    """
    try:
        size = os.path.getsize(path)
    except OSError:
        size = 0
    name = safe_filename(file_name) or os.path.basename(path)
    if media_root:
        rel = os.path.relpath(path, media_root).replace('\\', '/')
    else:
        rel = os.path.basename(path)
    return {
        'file_name': name,
        'storage_path': rel,
        'content_type': content_type or guess_content_type(path) or 'application/octet-stream',
        'size': size,
    }
