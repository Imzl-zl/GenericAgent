"""QQ 开放平台富媒体直发(2026-08-16 根路径媒体通道升级)。

QQ 官方无一步式发图接口, 整文件上传需公网 URL(根路径无公网入口)→ 走
官方 rich-media 分片 4 步流程(与平台 bot_poller QQAdapter 同构, 该实现
2026-08-14 审查 B2/I4 已过审; 依赖方向约束: poller import frontends,
此处必须独立实现不得反向 import):

  ① POST .../upload_prepare    → upload_id / block_size / parts[].presigned_url
  ② 逐片 PUT 预签名 URL        (幂等, 3 次退避重试)
  ③ 每片 PUT 后 POST .../upload_part_finish(upload_id/part_index/block_size/md5)
  ④ 全部分片完成后 POST .../files 合并 → file_info
  ⑤ 发媒体消息 msg_type=7 {media: {file_info}}

硬约束(官方 rich-media 文档 + 平台审查):
- 单聊/群聊上传不互通: 上传与发送必须同 endpoint 组(本模块由调用方传
  is_group 直接选组, 根路径入站时已知目标类型, 无需平台式兜底)。
- file_info 有 TTL: 必须在发送时刻上传(审查 B2)。
- upload_prepare / upload_part_finish 官方 10 QPS: 串行天然满足。
- 主动消息频控: 单聊 20 QPM(官方渠道矩阵), 留 25% 余量 → per-target
  令牌桶 15 QPM; 超限抛错, 调用方回退文本提示(内容不丢)。
- 失败语义: 任何一步失败抛 RuntimeError, 调用方回退"文件已生成但发送
  失败"提示——与 wxbot_client 失败回退原图同款语义, 不静默。

本模块只依赖 botpy(根路径已要求) + requests(PUT 分片), 无 PIL 依赖。
"""
import asyncio
import hashlib
import os
import threading
import time

import requests

#: QQ 媒体类型(官方 rich-media): 1=图片 2=视频 3=语音 4=文件
QQ_MEDIA_IMAGE = 1
QQ_MEDIA_VIDEO = 2
QQ_MEDIA_FILE = 4

#: md5_10m = 文件前 10002432 字节(约 9.54MB)的 MD5(官方明示, 平台 I4)。
_MD5_10M_BYTES = 10002432

#: 单聊主动消息频控 20 QPM(官方), 留 25% 余量。
_QQ_ACTIVE_QPM = 15


def _is_qq_rate_limit(resp):
    """官方 SDK isRateLimitError 同构(平台 poller 同款): HTTP 429 或
    err_code 50002(或消息含 rate limit)。限流是频控信号, 抛错让调用方
    fail-closed 回退提示, 不当作正常响应继续解析。"""
    if not isinstance(resp, dict):
        return False
    if str(resp.get('code', '')) == '429':
        return True
    if str(resp.get('err_code', '')) == '50002':
        return True
    msg = str(resp.get('message', '')) or str(resp.get('msg', ''))
    return 'rate limit' in msg.lower()


class _TokenBucket:
    """令牌桶节流(平台 poller 同款, 线程安全)。acquire 有界超时,
    超时返回 False 由调用方抛错/降级。"""

    def __init__(self, capacity, refill_per_sec):
        self._capacity = capacity
        self._tokens = float(capacity)
        self._refill_per_sec = refill_per_sec
        self._lock = threading.Lock()
        self._last = time.monotonic()

    def acquire(self, timeout=2.0):
        deadline = time.monotonic() + timeout
        while True:
            with self._lock:
                now = time.monotonic()
                self._tokens = min(
                    self._capacity,
                    self._tokens + (now - self._last) * self._refill_per_sec,
                )
                self._last = now
                if self._tokens >= 1:
                    self._tokens -= 1
                    return True
            if time.monotonic() >= deadline:
                return False
            time.sleep(min(0.05, max(0.0, deadline - time.monotonic())))


class QQMediaSender:
    """QQ 富媒体上传+发送(调度到 botpy 事件循环, 与平台 QQAdapter 同构)。

    client: 已连接的 botpy.Client 实例。方法为 async——在 botpy 回调
    (on_message 链)上下文里调用, 直接 await 其内部 _http.request。
    """

    def __init__(self, client):
        self._client = client
        self._buckets = {}  # target -> _TokenBucket(每关系独立预算)
        self._lock = threading.Lock()

    # -- 端点 ---------------------------------------------------------
    @staticmethod
    def _endpoint_set(is_group):
        """单聊/群聊上传不互通: 返回 (prepare, part_finish, files, messages) 四端点。"""
        if is_group:
            return (
                '/v2/groups/{group_openid}/upload_prepare',
                '/v2/groups/{group_openid}/upload_part_finish',
                '/v2/groups/{group_openid}/files',
                '/v2/groups/{group_openid}/messages',
            )
        return (
            '/v2/users/{openid}/upload_prepare',
            '/v2/users/{openid}/upload_part_finish',
            '/v2/users/{openid}/files',
            '/v2/users/{openid}/messages',
        )

    # -- 内部请求 ------------------------------------------------------
    async def _qq_request(self, method, route_template, target, payload=None):
        """调度 botpy 内部 HTTP(Route 模式, 平台 _qq_request 同构)。
        限流响应(429/50002)抛 RuntimeError——fail-closed, 调用方回退。"""
        from botpy.http import Route
        route = Route(method, route_template, openid=target, group_openid=target)
        http = self._client.api._http
        if payload is not None:
            resp = await http.request(route, json=payload)
        else:
            resp = await http.request(route)
        if _is_qq_rate_limit(resp):
            raise RuntimeError(f'qq rate limited: {resp.get("message") or resp.get("msg") or resp}')
        return resp or {}

    @staticmethod
    def _put_part_with_retry(presigned, data, part_idx):
        """分片 PUT 到预签名 URL: 最多 3 次退避重试(1s/2s), 仍失败抛错。
        PUT 幂等(同一 presigned_url 重 PUT 同内容)。"""
        last_exc = None
        for attempt in range(3):
            try:
                resp = requests.put(presigned, data=data, timeout=300)
                resp.raise_for_status()
                return
            except Exception as exc:
                last_exc = exc
                if attempt < 2:
                    time.sleep(1 * (2 ** attempt))
        raise RuntimeError(f'qq upload part {part_idx} failed after retries: {last_exc}')

    @staticmethod
    def _hashes(file_path):
        """md5/sha1/md5_10m 单遍流式计算(内存峰值 = 单分片缓冲, 平台 I4)。"""
        size = os.path.getsize(file_path)
        md5 = hashlib.md5()
        sha1 = hashlib.sha1()
        md5_10m = hashlib.md5()
        remaining = _MD5_10M_BYTES
        with open(file_path, 'rb') as f:
            while True:
                chunk = f.read(64 * 1024)
                if not chunk:
                    break
                md5.update(chunk)
                sha1.update(chunk)
                if remaining > 0:
                    take = min(remaining, len(chunk))
                    md5_10m.update(chunk[:take])
                    remaining -= take
        return md5.hexdigest(), sha1.hexdigest(), md5_10m.hexdigest()

    def _bucket(self, target):
        with self._lock:
            if len(self._buckets) >= 512:
                self._buckets.clear()
            bucket = self._buckets.get(target)
            if bucket is None:
                bucket = _TokenBucket(capacity=_QQ_ACTIVE_QPM,
                                      refill_per_sec=_QQ_ACTIVE_QPM / 60.0)
                self._buckets[target] = bucket
            return bucket

    # -- 上传+发送 -----------------------------------------------------
    async def send_media(self, target, file_path, file_name='', file_type=QQ_MEDIA_FILE,
                         is_group=False):
        """分片上传 → 发送媒体消息。失败抛 RuntimeError(调用方回退文本)。

        上传与发送必须同 endpoint 组(单聊上传的 file_info 只能发单聊)。
        """
        size = os.path.getsize(file_path)
        md5, sha1, md5_10m = self._hashes(file_path)
        prepare_tpl, part_finish_tpl, files_tpl, send_tpl = self._endpoint_set(is_group)
        name = file_name or os.path.basename(file_path)

        prepare = await self._qq_request('POST', prepare_tpl, target, {
            'file_type': file_type, 'file_size': str(size),
            'file_name': name,
            'md5': md5, 'sha1': sha1, 'md5_10m': md5_10m,
        })
        upload_id = str(prepare.get('upload_id') or '')
        parts = prepare.get('parts') or []
        block_size = int(prepare.get('block_size') or 0) or 5 * 1024 * 1024
        if not upload_id or not parts:
            raise RuntimeError('qq upload_prepare returned no upload_id/parts')

        for part in parts:
            idx = int(part.get('index', -1))
            if idx < 0:
                continue
            presigned = str(part.get('presigned_url') or '')
            if not presigned:
                raise RuntimeError(f'qq upload part {idx} missing presigned_url')
            part_block = int(part.get('block_size') or 0) or block_size
            offset = idx * block_size
            length = min(part_block, max(0, size - offset))
            if length <= 0:
                raise RuntimeError(f'qq upload part {idx} offset out of range')
            part_md5 = hashlib.md5()
            with open(file_path, 'rb') as f:
                f.seek(offset)
                data = f.read(length)
            part_md5.update(data)
            # ② PUT 分片(同步阻塞 300s 上限, 丢出事件循环)
            await asyncio.to_thread(self._put_part_with_retry, presigned, data, idx)
            # ③ 每片完成确认(官方: PUT 成功后必须通知, 否则合并失败)
            await self._qq_request('POST', part_finish_tpl, target, {
                'upload_id': upload_id, 'part_index': idx,
                'block_size': str(length), 'md5': part_md5.hexdigest(),
            })

        # ④ 合并(srv_send_msg=false 仅上传不直发, 发送走统一频控)
        finish = await self._qq_request('POST', files_tpl, target, {
            'upload_id': upload_id,
            'file_type': file_type,
            'file_name': name,
            'srv_send_msg': False,
        })
        file_info = str(finish.get('file_info') or '')
        if not file_info:
            raise RuntimeError('qq upload merge returned empty file_info')

        # ⑤ 发送(file_info 有 TTL, 上传完成后立即发)
        if not self._bucket(target).acquire(timeout=5.0):
            raise RuntimeError(f'qq active media rate limited for {target} ({_QQ_ACTIVE_QPM} QPM)')
        await self._qq_request('POST', send_tpl, target,
                               {'msg_type': 7, 'media': {'file_info': file_info}})
