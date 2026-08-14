"""Bot Poller: multi-channel IM gateway service (wechat/feishu/dingtalk/qq).

Architecture (IM_CHANNEL_BINDING §5):
    Go platform (control plane)  ──HTTP──▶  Bot Poller (this process)
         │                                    │
         │  /v1/im/webhook ◀──HTTP POST────  │  (inbound messages + media_paths)
         │                                    │
         └── /send ──▶ adapter.send_* ──▶ channel SDK

BotAdapter 注册表: channel_type → adapter 工厂。每个活跃 channel_configs
行(bot_uuid)在独立线程运行:
    WeChatAdapter   = WxBotClient iLink 长轮询(现有逻辑迁移, 读 config {token})
    FeishuAdapter   = lark-oapi WebSocket 长连接(config {app_id, app_secret})
    DingTalkAdapter = dingtalk-stream 长连接(config {app_id→app_key, app_secret})
    QQAdapter       = botpy WebSocket(config {app_id, app_secret})

SDK 惰性导入(import 时缺失只在该渠道启动时报错, 不影响其他渠道/测试)。

群消息触发 = 渠道平台协议规则(IM_CHANNEL_BINDING §2, 非本服务选择):
钉钉/QQ 平台只推送 @ 机器人的消息(GROUP_AT_MESSAGE_CREATE); 飞书申请
group_at_msg 权限(仅 @, 不申请收全部的敏感权限 group_msg)。

入站统一 POST /v1/im/webhook, body 契约:
    {bot_uuid, channel_type, channel_account_id, conversation_id,
     message_id, text, updates_buf(微信), context_token(微信),
     media_paths/media_items(微信), source_message_ids}
conversation_id 取值: QQ=group_openid/openid、钉钉=conversationId、飞书=
chat_id、微信恒空(单桶)。
"""

import argparse
import asyncio
import collections
import hashlib
import hmac
import json
import os
import sys
import threading
import time
import uuid
from abc import ABC, abstractmethod
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import requests

# Make frontends/ importable for wxbot_client and wxbot_media, and make
# bot_poller/ importable for media_downloader (sibling module).
# _POLLER_DIR = tenant_platform/bot_poller
# _LEGACY_ROOT = GenericAgent (two levels up)
# _FRONTENDS_DIR = GenericAgent/frontends (where wxbot_client.py and wxbot_media.py live)
_POLLER_DIR = os.path.dirname(os.path.abspath(__file__))
_LEGACY_ROOT = os.path.dirname(os.path.dirname(_POLLER_DIR))
_FRONTENDS_DIR = os.path.join(_LEGACY_ROOT, 'frontends')
for _p in (_POLLER_DIR, _FRONTENDS_DIR, _LEGACY_ROOT):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import media_downloader as media_dl  # noqa: E402
from wxbot_client import WxBotClient, AuthExpired, fit_image_for_upload  # noqa: E402
from wxbot_media import download_media  # noqa: E402

# 扩展名 → MIME 映射迁移至 media_downloader(多渠道共用, 2026-08-13 审查
# 收敛): 保留名字兼容 WeChatAdapter._collect_media_items 与既有测试。
_EXT_CONTENT_TYPES = media_dl.EXT_CONTENT_TYPES


def _guess_content_type(file_name):
    """Infer MIME type from file extension. Defaults to octet-stream."""
    return media_dl.guess_content_type(file_name)

POLL_TIMEOUT = 30
WEBHOOK_TIMEOUT = 10

# 渠道类型常量(与 Go domain.ChannelType 对齐)。
CHANNEL_WECHAT = 'wechat'
CHANNEL_FEISHU = 'feishu'
CHANNEL_DINGTALK = 'dingtalk'
CHANNEL_QQ = 'qq'
CHANNEL_WECOM = 'wecom'
VALID_CHANNEL_TYPES = {CHANNEL_WECHAT, CHANNEL_FEISHU, CHANNEL_DINGTALK, CHANNEL_QQ, CHANNEL_WECOM}


def _env_int(name, default):
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        print(f"WARNING: {name}={raw!r} is not an integer; using default {default}", flush=True)
        return default


# 控制面 HTTP 请求体上限(审查: 此前无限流, 与 Go 侧 bodyLimitMiddleware 不对称)。
# 默认 1 MiB 与 Go 侧 DefaultMaxRequestBodyBytes 一致; 控制面只传 JSON
# (bot_token/webhook_url/file_path 路径字符串), 不传文件内容本身——部署可按需
# 经 BOT_POLLER_MAX_BODY_BYTES 调大(与 PLATFORM_MAX_BODY_BYTES 对齐)。
MAX_BODY_BYTES = _env_int("BOT_POLLER_MAX_BODY_BYTES", 1024 * 1024)
# ThreadingHTTPServer 每连接一线程且默认无 socket 超时——慢速/死连接会无限占线
# 程。设置读超时让空闲连接被回收(审查 I-1)。
HTTP_READ_TIMEOUT = 30.0
HTTP_REQUEST_QUEUE_SIZE = 64
# Webhook delivery retry: exponential backoff base/cap. Retrying blocks the
# adapter's dispatch loop on purpose — that is the backpressure that stops the
# cursor from advancing past an undelivered message (same model as a Kafka
# consumer that refuses to commit an offset it failed to process).
WEBHOOK_RETRY_BASE_SECONDS = 2.0
WEBHOOK_RETRY_CAP_SECONDS = 60.0
MAX_INBOUND_COALESCE_WINDOW_MS = 5000
MAX_COALESCED_MESSAGES = 8
# 入站媒体字节保留期(2026-08-13 审查 I4/D7): 媒体=用户隐私数据,
# media_root/<bot_uuid>/ 下的下载文件按 mtime 周期性清扫, 与 Go 侧
# media_assets 审计行 90d 保留期对齐。env 可调。
MEDIA_RETENTION_DAYS = _env_int('BOT_POLLER_MEDIA_RETENTION_DAYS', 90)
# 清扫节流(daemon 线程每 60s tick 检查, 满 24h 执行一次)。
MEDIA_SWEEP_TICK_SECONDS = 60
MEDIA_SWEEP_INTERVAL_SECONDS = 24 * 3600

def _parse_listen_addr(listen):
    """Parse 'host:port' into (host, port_int). Raises ValueError on bad input."""
    if ':' not in listen:
        raise ValueError(f"listen address must be 'host:port', got: {listen}")
    host, port_str = listen.rsplit(':', 1)
    return host, int(port_str)


# msg_type values accepted by /send. "text" is the default for backward compat.
MSG_TYPE_TEXT = 'text'
MSG_TYPE_IMAGE = 'image'
MSG_TYPE_VIDEO = 'video'
MSG_TYPE_FILE = 'file'
_VALID_MSG_TYPES = {MSG_TYPE_TEXT, MSG_TYPE_IMAGE, MSG_TYPE_VIDEO, MSG_TYPE_FILE}

# 出站媒体类型(与 /send msg_type 对齐, IM_MEDIA_ARCHITECTURE §5.1 A1)。
VALID_MEDIA_TYPES = (MSG_TYPE_IMAGE, MSG_TYPE_FILE, MSG_TYPE_VIDEO)


def _message_time_ms(msg, fallback_ms):
    for key in ('create_time_ms', 'create_time', 'timestamp', 'message_time'):
        value = msg.get(key)
        if isinstance(value, (int, float)) and value > 0:
            if key == 'create_time_ms':
                return int(value)
            return int(value if value >= 1_000_000_000_000 else value * 1000)
    return fallback_ms


def _is_command_body(body):
    return str(body.get('text') or '').lstrip().startswith('/')


def _can_coalesce(previous, current, window_ms):
    if _is_command_body(previous) or _is_command_body(current):
        return False
    if previous.get('bot_uuid') != current.get('bot_uuid'):
        return False
    if previous.get('channel_account_id') != current.get('channel_account_id'):
        return False
    if previous.get('channel_type') != current.get('channel_type'):
        return False
    if previous.get('conversation_id') != current.get('conversation_id'):
        return False
    previous_at = int(previous.get('_received_at_ms') or 0)
    current_at = int(current.get('_received_at_ms') or 0)
    # 时间接近即合并(窗口语义), 不要求严格递增: 微信"文件+文字"同时发送时,
    # iLink 批次内顺序与 create_time 顺序可能相反(文件上传耗时), 严格递增
    # 会把同一发送动作拆成两个任务(用户侧表现为"回复两个")。
    return abs(current_at - previous_at) <= window_ms


def _finalize_coalesced_group(group):
    source_ids = [str(item.get('message_id') or '') for item in group]
    if len(group) == 1:
        out = dict(group[0])
    else:
        digest = hashlib.sha256('|'.join(source_ids).encode('utf-8')).hexdigest()[:32]
        out = dict(group[0])
        out['message_id'] = f'coalesced:{digest}'
        out['text'] = '\n'.join(
            str(item.get('text') or '').strip()
            for item in group
            if str(item.get('text') or '').strip()
        )
        out['media_paths'] = list(dict.fromkeys(
            path
            for item in group
            for path in (item.get('media_paths') or [])
            if path
        ))
        seen_storage = set()
        media_items = []
        for item in group:
            for media in item.get('media_items') or []:
                storage_path = str(media.get('storage_path') or '')
                key = storage_path or json.dumps(media, sort_keys=True, ensure_ascii=False)
                if key in seen_storage:
                    continue
                seen_storage.add(key)
                media_items.append(media)
        out['media_items'] = media_items
        out['updates_buf'] = group[-1].get('updates_buf', '')
        out['context_token'] = group[-1].get('context_token', '')
    out['source_message_ids'] = source_ids
    out.pop('_received_at_ms', None)
    return out


def coalesce_webhook_bodies(bodies):
    """Finalize each webhook body as its own group (恒等路径, 审查 C2)。

    跨批次窗口合并由 InboundCoalescingBuffer 承担(window>0 时)——旧版
    window>0 分组分支是生产不可达死代码(唯一调用点走恒等路径), 却由
    7 个测试驱动, 维护两套合并语义; 已删除, 测试改挂到生产实现。
    """
    return [_finalize_coalesced_group([body]) for body in bodies]


class InboundCoalescingBuffer:
    """Retains one user's adjacent messages across get_updates batches."""

    def __init__(self, window_ms):
        self._window_ms = max(0, int(window_ms))
        self._pending = []
        self._deadline_ms = None

    def set_window(self, window_ms):
        self._window_ms = max(0, int(window_ms))

    def push(self, bodies, now_ms):
        if self._window_ms <= 0:
            ready = self.flush_all()
            ready.extend(coalesce_webhook_bodies(bodies))
            return ready

        ready = []
        for body in bodies:
            if _is_command_body(body):
                ready.extend(self.flush_all())
                ready.append(_finalize_coalesced_group([body]))
                continue
            if self._pending and (
                len(self._pending) >= MAX_COALESCED_MESSAGES
                or not _can_coalesce(self._pending[-1], body, self._window_ms)
            ):
                ready.extend(self.flush_all())
            self._pending.append(body)
            self._deadline_ms = int(now_ms) + self._window_ms
        return ready

    def flush_due(self, now_ms):
        if not self._pending:
            return []
        if self._window_ms <= 0 or int(now_ms) >= self._deadline_ms:
            return self.flush_all()
        return []

    def flush_all(self):
        if not self._pending:
            return []
        group = self._pending
        self._pending = []
        self._deadline_ms = None
        return [_finalize_coalesced_group(group)]

    def timeout_seconds(self, now_ms):
        if not self._pending or self._deadline_ms is None:
            return None
        return max(0.05, (self._deadline_ms - int(now_ms)) / 1000.0)


class _DedupWindow:
    """Bounded FIFO dedup window: O(1) membership via set, FIFO eviction via deque.

    Single-threaded use only (each bot thread owns its own instance).
    """

    def __init__(self, maxlen=2000):
        self._maxlen = maxlen
        self._set = set()
        self._fifo = collections.deque()

    def add(self, key):
        """Add key; returns True if new, False if already present."""
        if key in self._set:
            return False
        self._set.add(key)
        self._fifo.append(key)
        if len(self._fifo) > self._maxlen:
            self._set.discard(self._fifo.popleft())
        return True


class _TokenBucket:
    """令牌桶节流(飞书官方 5 QPS 上限, IM_STREAMING_DELIVERY 决策 3)。

    acquire 阻塞等待令牌(有界超时): 超时返回 False 由调用方抛错/降级。
    线程安全。"""

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
            time.sleep(0.05)


class _QQRateLimited(Exception):
    """QQ 流式帧限流(HTTP 429 / err_code 50002)——重试信号。"""


def _is_qq_rate_limit(resp):
    """官方 SDK isRateLimitError 同构: HTTP 429 或 err_code 50002。"""
    if not isinstance(resp, dict):
        return False
    if str(resp.get('code', '')) == '429':
        return True
    if str(resp.get('err_code', '')) == '50002':
        return True
    msg = str(resp.get('message', '')) or str(resp.get('msg', ''))
    return 'rate limit' in msg.lower()


class BotAdapter(ABC):
    """Base class for one channel connection (IM_CHANNEL_BINDING §5).

    每个活跃 channel_configs 行一个 adapter 实例, 独立线程运行连接循环。
    入站消息统一走 post_webhook(body) → /v1/im/webhook(HMAC 签名, 重试
    契约与旧 wechat 实现一致)。
    """

    #: 渠道类型(子类覆盖), 与 channel_configs.channel_type 对齐。
    channel_type = ''
    #: 流式能力开关(IM_STREAMING_DELIVERY §4.3): 飞书/QQ 单聊实现,
    #: 钉钉/微信不实现(非流渠道保持 SendMessage)。
    stream_supported = False

    def __init__(self, bot_uuid, webhook_url, media_root=None, webhook_secret='',
                 coalesce_window_provider=None):
        self.bot_uuid = bot_uuid
        self.webhook_url = webhook_url
        self.stop_event = threading.Event()
        self.thread = None
        self.webhook_idle = threading.Event()
        self.webhook_idle.set()
        self._webhook_secret = webhook_secret or ''
        self._media_root = media_root
        self._media_dir = None
        if media_root:
            self._media_dir = os.path.join(media_root, bot_uuid)
            os.makedirs(self._media_dir, exist_ok=True)
        # 入站窗口合并(2026-08-14 审查 I-3): 全部渠道共用同一缓冲语义——
        # 窗口内同一会话相邻消息(图 + 后续文本)合并为一个任务, 修复
        # “图消息与后续文本拆成两个任务、文本任务 media=null”的追问语义
        # 断裂。窗口 = 平台 im_inbound_coalesce_window_ms(默认 2500ms,
        # 微信已在用)。事件渠道(QQ/飞书/钉钉/企微)由定时器驱动窗口到期
        # flush, 微信由长轮询循环驱动(无定时器竞态)。所有 flush 路径共用
        # _flush_lock, 防并发双投。命令消息 push 即 flush 不延迟。
        self._coalesce_window_provider = coalesce_window_provider
        self._coalescer = InboundCoalescingBuffer(0)
        self._flush_lock = threading.Lock()
        self._flush_timer = None

    def _arm_flush_timer(self):
        """按窗口剩余时间启动/重置 flush 定时器(事件渠道专用)。"""
        delay = self._coalescer.timeout_seconds(int(time.time() * 1000))
        if delay is None:
            return
        if self._flush_timer is not None:
            self._flush_timer.cancel()
        self._flush_timer = threading.Timer(delay, self._flush_tick)
        self._flush_timer.daemon = True
        self._flush_timer.start()

    def _flush_tick(self):
        """定时器到期: 锁内取到期组, 锁外投递(避免持锁网络阻塞)。"""
        with self._flush_lock:
            self._flush_timer = None
            ready = self._coalescer.flush_due(int(time.time() * 1000))
        for body in ready:
            self.post_webhook(body)

    def deliver_inbound(self, bodies, now_ms=None):
        """入站 body(s) 投递: 窗口合并 → 定时 flush → post_webhook。

        window<=0 时等价于直接逐条投递(零延迟, 与旧行为一致); window>0
        时窗口内相邻消息合并为一组, 窗口到期整组投递。微信 _dispatch_batch
        与四渠道事件处理共用此入口(微信另有长轮询循环驱动 flush)。"""
        with self._flush_lock:
            window = int(self._coalesce_window_provider() or 0) if self._coalesce_window_provider else 0
            self._coalescer.set_window(window)
            now = int(now_ms) if now_ms is not None else int(time.time() * 1000)
            ready = self._coalescer.push(bodies, now)
            ready.extend(self._coalescer.flush_due(now))
            if window > 0 and self._coalescer.timeout_seconds(now) is not None:
                self._arm_flush_timer()
        for body in ready:
            self.post_webhook(body)

    # -- 生命周期 ----------------------------------------------------------
    def start(self):
        """Spawn the connection thread. Idempotent."""
        if self.thread is not None and self.thread.is_alive():
            return
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def stop(self):
        """Request shutdown and wait for the connection thread. Returns the
        committed updates_buf for wechat ('' for other channels)."""
        self.stop_event.set()
        self.webhook_idle.wait(timeout=WEBHOOK_TIMEOUT + 1)
        with self._flush_lock:
            if self._flush_timer is not None:
                self._flush_timer.cancel()
                self._flush_timer = None
        if self.thread is not None:
            self.thread.join(timeout=5)
        return ''

    @abstractmethod
    def _run(self):
        """Connection loop; exits on stop_event or fatal auth error."""

    # -- 出站 --------------------------------------------------------------
    @abstractmethod
    def send_text(self, target, text, client_id=''):
        """Send a text reply to target (微信=ilink_user_id, 新渠道=对话单元)。"""

    # -- 出站媒体统一接口(IM_MEDIA_ARCHITECTURE §5.1 A1) -------------------
    # media_type ∈ image|file|video。基类提供分发骨架 + 防御校验; 子类实现
    # "上传 + 发送"薄适配(_send_image/_send_file/_send_video)。不支持/未实现
    # 抛 NotImplementedError → Go 侧 delivery 走既有错误路径(fail-closed,
    # 决策 A3: 不静默降级文本)。
    # media_size_limit: 防御性单文件上限, 对齐 Go delivery_safety per-type
    # 上限(delivery_capture.go maxDeliverableBytesByType: image ≤20MiB /
    # video ≤100MiB / file ≤8MiB——捕获层已限, 此处按类型分档的纵深防御,
    # 决策 A4/T5)。None = 按 media_type 分档; 子类/测试可覆盖为固定值。
    media_size_limit = None
    _MEDIA_SIZE_LIMITS = {'image': 20 << 20, 'video': 100 << 20, 'file': 8 << 20}

    def send_media(self, target, file_path, media_type, file_name='', client_id=''):
        """统一出站媒体入口: 校验 → 按 media_type 分发到 _send_*。"""
        if media_type not in VALID_MEDIA_TYPES:
            raise ValueError(f'unsupported media_type: {media_type}')
        if not file_path or not os.path.isfile(file_path):
            raise ValueError(f'media file not found: {file_path}')
        limit = self.media_size_limit
        if limit is None:
            limit = self._MEDIA_SIZE_LIMITS.get(media_type, 8 << 20)
        if limit > 0 and os.path.getsize(file_path) > limit:
            raise ValueError(
                f'media {file_path} exceeds size limit {limit}')
        handler = {'image': self._send_image,
                   'file': self._send_file,
                   'video': self._send_video}[media_type]
        return handler(target, file_path, file_name=file_name, client_id=client_id)

    def _send_image(self, target, file_path, file_name='', client_id=''):
        raise NotImplementedError(
            f'send_media(image) not supported by {self.channel_type} adapter')

    def _send_file(self, target, file_path, file_name='', client_id=''):
        raise NotImplementedError(
            f'send_media(file) not supported by {self.channel_type} adapter')

    def _send_video(self, target, file_path, file_name='', client_id=''):
        raise NotImplementedError(
            f'send_media(video) not supported by {self.channel_type} adapter')

    # -- IM 流式输出(IM_STREAMING_DELIVERY §4.3) ---------------------------
    # 非流渠道(钉钉/微信)不实现: 基类默认抛错, Go 侧 BeginReply 失败后
    # 回退终态 delivery。流式渠道实现 open/append/commit/abort。
    def send_stream_open(self, target, text=''):
        raise NotImplementedError(
            f'send_stream_open not supported by {self.channel_type} adapter (final-only)')

    def send_stream_append(self, stream_id, text):
        raise NotImplementedError(
            f'send_stream_append not supported by {self.channel_type} adapter (final-only)')

    def send_stream_commit(self, stream_id, text=''):
        raise NotImplementedError(
            f'send_stream_commit not supported by {self.channel_type} adapter (final-only)')

    def send_stream_abort(self, stream_id):
        raise NotImplementedError(
            f'send_stream_abort not supported by {self.channel_type} adapter (final-only)')

    # -- 入站 webhook 投递 -------------------------------------------------
    def post_webhook(self, body, max_attempts=None):
        """POST an inbound message body with deterministic JSON + HMAC.

        契约与旧 wechat 实现一致(见 _post_webhook_body_inner):
        2xx 成功; 4xx 永久拒绝丢弃; 5xx/网络错误指数退避重试, 阻塞本渠道
        dispatch(背压保持顺序, 防 cursor 越过未投递消息)。
        """
        self.webhook_idle.clear()
        try:
            delivered = self._post_webhook_inner(body, max_attempts=max_attempts)
            if delivered:
                self._on_webhook_delivered(body)
            return delivered
        finally:
            self.webhook_idle.set()

    def _on_webhook_delivered(self, body):
        """2xx 后的钩子(微信提交 cursor; 其他渠道无状态)。"""

    def _post_webhook_inner(self, body, max_attempts=None):
        body_bytes = json.dumps(body, separators=(',', ':')).encode('utf-8')
        headers = {'Content-Type': 'application/json'}
        if self._webhook_secret:
            sig = hmac.new(
                self._webhook_secret.encode('utf-8'),
                body_bytes,
                hashlib.sha256,
            ).hexdigest()
            headers['X-Webhook-Signature'] = sig

        attempt = 0
        while not self.stop_event.is_set():
            attempt += 1
            try:
                resp = requests.post(self.webhook_url, data=body_bytes,
                                     headers=headers, timeout=WEBHOOK_TIMEOUT)
                if 200 <= resp.status_code < 300:
                    return True
                if 400 <= resp.status_code < 500:
                    print(f'[Poller] webhook PERMANENTLY rejected ({self.bot_uuid}) '
                          f'status={resp.status_code} body={resp.text[:200]} — message dropped',
                          flush=True)
                    return False
                err_desc = f'status={resp.status_code} body={resp.text[:200]}'
            except Exception as exc:
                err_desc = f'error={exc}'

            if max_attempts is not None and attempt >= max_attempts:
                print(f'[Poller] webhook delivery gave up after {attempt} attempts '
                      f'({self.bot_uuid}): {err_desc}', flush=True)
                return False
            backoff = min(WEBHOOK_RETRY_BASE_SECONDS * (2 ** (attempt - 1)),
                          WEBHOOK_RETRY_CAP_SECONDS)
            print(f'[Poller] webhook post failed ({self.bot_uuid}) attempt={attempt} '
                  f'{err_desc}; retrying in {backoff:.0f}s', flush=True)
            self.stop_event.wait(backoff)
        return False

    # -- 工具 --------------------------------------------------------------
    def webhook_body(self, *, channel_account_id, conversation_id, message_id, text,
                     updates_buf='', context_token='', media_paths=None, media_items=None,
                     source_message_ids=None, received_at_ms=0, conversation_type='private'):
        """Build one platform webhook body (IM_CHANNEL_BINDING §5 契约)。

        conversation_type: 'private' | 'group'——IM 流式转发判定维度
        (IM_STREAMING_DELIVERY §4.4: 群聊统一只发最终结果)。各 adapter
        有现成信息: QQ is_group、飞书 chat_type、钉钉 conversation_type。
        """
        body = {
            'bot_uuid': self.bot_uuid,
            'channel_type': self.channel_type,
            'channel_account_id': channel_account_id,
            'conversation_id': conversation_id,
            'conversation_type': conversation_type,
            'message_id': message_id,
            'text': text,
            'context_token': context_token,
            'updates_buf': updates_buf,
            'media_paths': media_paths or [],
            'media_items': media_items or [],
        }
        if received_at_ms:
            body['_received_at_ms'] = received_at_ms
        if source_message_ids:
            body['source_message_ids'] = source_message_ids
        return body


class WeChatAdapter(BotAdapter):
    """iLink official gateway long-poll adapter (现有 WxBotClient 逻辑迁移)。

    config: {token}; 附加 base_url / updates_buf 经构造参数注入。仅私聊
    单桶(IM_CHANNEL_ARCHITECTURE §2): conversation_id 恒空, 回复目标 =
    ilink_user_id。
    """

    channel_type = CHANNEL_WECHAT

    def _on_webhook_delivered(self, body):
        """提交 cursor: 与旧 BotEntry 语义一致(2xx 后推进 committed)。"""
        committed_cursor = body.get('updates_buf', '')
        if committed_cursor:
            self.committed_updates_buf = committed_cursor

    def __init__(self, bot_uuid, config, webhook_url, *, base_url='', updates_buf='',
                 media_root=None, webhook_secret='', coalesce_window_provider=None):
        super().__init__(bot_uuid, webhook_url, media_root=media_root, webhook_secret=webhook_secret,
                         coalesce_window_provider=coalesce_window_provider)
        token = (config or {}).get('token') or ''
        self.client = WxBotClient(token=token, persist=False, base_url=base_url or None)
        self.committed_updates_buf = updates_buf or getattr(self.client, 'updates_buf', '') or ''
        # 合并缓冲由基类统一(_coalescer, 审查 I-3): 微信由长轮询循环驱动
        # flush + 定时器双保险(锁保证不双投)。

    def _run(self):
        """Long-poll loop. Exits on stop_event or AuthExpired."""
        seen = _DedupWindow(maxlen=2000)
        while not self.stop_event.is_set():
            try:
                now_ms = int(time.time() * 1000)
                ready = []
                with self._flush_lock:
                    if self._coalesce_window_provider:
                        self._coalescer.set_window(self._coalesce_window_provider())
                    ready = self._coalescer.flush_due(now_ms)
                    request_timeout = self._coalescer.timeout_seconds(now_ms)
                for body in ready:
                    self.post_webhook(body)
                messages = self.client.get_updates(
                    POLL_TIMEOUT, request_timeout=request_timeout
                )
                self._dispatch_batch(messages, seen)
            except AuthExpired:
                self._notify_expired()
                break
            except Exception as exc:  # network jitter: back off and retry
                print(f'[Poller] bot {self.bot_uuid} err: {exc}', flush=True)
                self.stop_event.wait(5)

    def _dispatch_batch(self, messages, seen):
        received_at_ms = int(time.time() * 1000)
        bodies = []
        for msg in messages:
            body = self._prepare_webhook_body(msg, seen, received_at_ms)
            if body is not None:
                bodies.append(body)
        # 审查 I-3: 统一走基类合并投递(与四渠道同语义)。
        self.deliver_inbound(bodies, received_at_ms)

    def _prepare_webhook_body(self, msg, seen, fallback_time_ms):
        """Download media and build one platform webhook body."""
        mid = str(msg.get('message_id', 0))
        if not self.client.is_user_msg(msg) or not seen.add(mid):
            return None
        text = self.client.extract_text(msg)
        uid = msg.get('from_user_id', '')
        ctx = msg.get('context_token', '')
        media_paths = []
        media_items = []
        if self._media_dir:
            try:
                # Round8: with_names 返回 (path, 原始 file_name) 同序对, 避免
                # 按位置索引 item_list 错位(下载失败的项会使索引偏移)。
                downloaded = download_media(msg.get('item_list', []), dest_dir=self._media_dir, with_names=True)
                media_paths = [p for p, _ in downloaded]
                media_names = [n for _, n in downloaded]
                media_items = self._collect_media_items(media_paths, media_names)
            except Exception as exc:
                print(f'[Poller] media dl err ({self.bot_uuid}): {exc}', flush=True)
        return self.webhook_body(
            channel_account_id=uid,
            conversation_id='',  # 微信个人自用单桶(IM_CHANNEL_ARCHITECTURE §2)
            message_id=mid,
            text=text,
            updates_buf=self.client.updates_buf,
            context_token=ctx,
            media_paths=media_paths,
            media_items=media_items,
            received_at_ms=_message_time_ms(msg, fallback_time_ms),
        )

    def _collect_media_items(self, paths, names=None):
        """Build metadata for each downloaded media file.

        Returns list of dicts with file_name, storage_path (relative to
        media_root), content_type, size. storage_path uses forward slashes
        for cross-platform compatibility (Windows backslashes normalized)
        so the same DB row works when media_root is re-pointed to NFS or
        an S3 mount.
        """
        if not paths:
            return []
        items = []
        for idx, path in enumerate(paths):
            try:
                size = os.path.getsize(path)
            except OSError:
                size = 0
            # Round8 审查: 落盘名含内容 hash 前缀(防同名覆盖), 用户可见名
            # 恢复为发送者原始 file_name。names 由 download_media(with_names)
            # 与 paths 同序产出, 不得按位置索引原始 item_list(下载失败项
            # 会使索引错位)。
            file_name = os.path.basename(path)
            if names is not None and idx < len(names) and names[idx]:
                file_name = names[idx]
            if self._media_root:
                rel = os.path.relpath(path, self._media_root)
            else:
                rel = file_name
            storage_path = rel.replace('\\', '/')
            items.append({
                'file_name': file_name,
                'storage_path': storage_path,
                'content_type': _guess_content_type(file_name),
                'size': size,
            })
        return items

    def _notify_expired(self):
        body = {'bot_uuid': self.bot_uuid, 'auth_expired': True}
        # Bounded attempts: the bot loop is exiting either way; the platform
        # also detects expiry via send failures, so losing this signal is
        # recoverable and must not wedge the thread forever.
        self.post_webhook(body, max_attempts=5)

    def stop(self):
        super().stop()
        return self.committed_updates_buf

    def send_text(self, target, text, client_id=''):
        self.client.send_text(target, text, context_token='', client_id=client_id)

    # 统一出站媒体接口(IM_MEDIA_ARCHITECTURE §5.1 A1): iLink 直传无上传步骤,
    # 直接委托 WxBotClient(2026-08-14 独立审查清理: 旧 send_file/send_image/
    # send_video 双签名兼容层无任何调用方, 已删除; send_media 统一分发)。
    def _send_image(self, target, file_path, file_name='', client_id=''):
        self.client.send_image(target, file_path, context_token='', client_id=client_id)

    def _send_file(self, target, file_path, file_name='', client_id=''):
        self.client.send_file(target, file_path, context_token='', file_name=file_name, client_id=client_id)

    def _send_video(self, target, file_path, file_name='', client_id=''):
        self.client.send_video(target, file_path, context_token='', client_id=client_id)



class FeishuAdapter(BotAdapter):
    """飞书企业自建应用(lark-oapi WebSocket)。

    config: {app_id, app_secret}。订阅 p2_im.message.receive_v1(含 p2p 与
    @群消息——权限申请 group_at_msg, 不申请收全部的敏感权限 group_msg)。
    conversation_id = chat_id(p2p/group 均为现成字段); 回复目标 = chat_id。
    """

    channel_type = CHANNEL_FEISHU
    #: 飞书支持消息编辑打字机(IM_STREAMING_DELIVERY §4.3): 发占位消息拿
    #: message_id, append 走 PUT /im/v1/messages/:id 全量更新, commit 最后一次
    #: 更新, abort 改写"生成中断"提示。
    stream_supported = True
    #: 官方硬限制: 一条消息最多编辑 20 次(open.feishu.cn im-v1 message update
    #: Limitation of Use)。append 每帧一次 PUT + commit 一次 PUT ≤ 20;
    #: 留 1 帧余量给 commit, append 最多 18 次。超过后文本继续累积,
    #: 由 commit 最后一次更新一次性送达(打字机冻结, 内容不丢)。
    _MAX_STREAM_EDITS = 18

    def __init__(self, bot_uuid, config, webhook_url, *, media_root=None, webhook_secret='',
                 coalesce_window_provider=None):
        super().__init__(bot_uuid, webhook_url, media_root=media_root, webhook_secret=webhook_secret,
                         coalesce_window_provider=coalesce_window_provider)
        self._app_id = (config or {}).get('app_id') or ''
        self._app_secret = (config or {}).get('app_secret') or ''
        self._ws_client = None
        self._api_client = None  # 出站 API client(惰性)
        self._sdk_error = None
        # 流式状态: stream_id -> {target, message_id, text(累积)}
        self._streams = {}
        self._stream_lock = threading.Lock()
        # 飞书官方频控: 同用户/同群 5 QPS(IM_STREAMING_DELIVERY 渠道矩阵)。
        # platform 侧 500ms 节流合并已天然 ≤2 QPS, 此处是防御性上限。
        self._throttle = _TokenBucket(capacity=5, refill_per_sec=5)
        try:
            import lark_oapi as lark  # 惰性导入: SDK 缺失只影响本渠道
            self._lark = lark
        except Exception as exc:  # pragma: no cover - 环境缺失路径
            self._sdk_error = exc

    def _build_handler(self):
        lark = self._lark

        def handle_message(data):
            self._handle_feishu_message(data)

        return lark.EventDispatcherHandler.builder('', '').register_p2_im_message_receive_v1(handle_message).build()

    def _handle_feishu_message(self, data):
        """飞书入站消息 → webhook body。conversation_id = chat_id(p2p/group
        统一); 群消息仅 @ 触发(权限申请 group_at_msg, 不申请收全部的敏感
        权限 group_msg——IM_CHANNEL_BINDING §2)。"""
        event = getattr(data, 'event', None)
        message = getattr(event, 'message', None)
        sender = getattr(event, 'sender', None)
        if message is None:
            return
        message_id = getattr(message, 'message_id', '') or ''
        chat_id = getattr(message, 'chat_id', '') or ''
        sender_id = ''
        sender_obj = getattr(sender, 'sender_id', None)
        if sender_obj is not None:
            sender_id = getattr(sender_obj, 'open_id', '') or ''
        if not message_id:
            return
        text = ''
        media_paths, media_items = [], []
        mtype = getattr(message, 'message_type', '') or ''
        if mtype == 'text':
            try:
                content = json.loads(getattr(message, 'content', '') or '{}')
                text = str(content.get('text', ''))
            except (ValueError, TypeError):
                text = ''
        elif mtype in ('image', 'file', 'audio', 'media') and self._media_dir:
            # 入站媒体提取(IM_MEDIA_ARCHITECTURE §3.1): image → image_key;
            # file/audio/media(视频) → file_key。下载走
            # GET /im/v1/messages/{message_id}/resources/{file_key}?type=…
            # (官方: file_key 与 message_id 必须匹配, ≤100MB, 审查 S5)。
            try:
                content = json.loads(getattr(message, 'content', '') or '{}')
            except (ValueError, TypeError):
                content = {}
            file_key = str(content.get('image_key') or content.get('file_key') or '')
            file_name = str(content.get('file_name') or '') or file_key
            resource_type = 'image' if mtype == 'image' else 'file'
            if file_key and message_id:
                try:
                    data = self._download_resource(message_id, file_key, resource_type)
                    if data:
                        path = media_dl.save_bytes_bounded(
                            data, self._media_dir, file_name=file_name)
                        media_paths.append(path)
                        media_items.append(media_dl.build_media_item(
                            path, self._media_root, file_name))
                except Exception as exc:
                    print(f'[Poller] feishu media dl err ({self.bot_uuid}): {exc}', flush=True)
        if not text and not media_paths:
            # 无文本且媒体提取失败(下载/落盘异常): 丢弃并记日志, 不回
            # 误导性的 "empty message ignored"(审查 B1 统一行为)。
            return
        self.deliver_inbound([self.webhook_body(
            channel_account_id=sender_id,
            conversation_id=chat_id,  # p2p/group 统一 chat_id
            message_id=message_id,
            text=text,
            media_paths=media_paths,
            media_items=media_items,
            conversation_type=self._conversation_type(data),
        )])

    def _download_resource(self, message_id, file_key, resource_type):
        """飞书消息资源下载: GET /im/v1/messages/{id}/resources/{key}?type=…。

        返回二进制 bytes。走独立 API client(与出站同源, WS 通道只收不发)。
        resource_type: 'image' | 'file'(音频/视频/文件均走 file)。
        """
        v1 = self._lark.api.im.v1
        api = self._ensure_api_client()
        req = (v1.GetMessageResourceRequest.builder()
               .message_id(message_id)
               .file_key(file_key)
               .type(resource_type)
               .build())
        resp = api.im.v1.message.get_resource(req)
        if not resp.success():
            raise RuntimeError(
                f'feishu resource get failed: code={resp.code} msg={resp.msg}')
        data = getattr(resp, 'file', None) or b''
        if not data:
            raise RuntimeError('feishu resource get returned empty body')
        return data

    @staticmethod
    def _conversation_type(data):
        """群/私聊判定: message.chat_type('p2p'|'group')。缺省回退 private。"""
        try:
            chat_type = getattr(getattr(data, 'event', None), 'message', None)
            chat_type = getattr(chat_type, 'chat_type', '') or ''
            if chat_type == 'group':
                return 'group'
        except Exception:
            pass
        return 'private'

    def _run(self):
        if self._sdk_error is not None:
            print(f'[Poller] feishu {self.bot_uuid}: lark_oapi unavailable: {self._sdk_error}', flush=True)
            return
        if not self._app_id or not self._app_secret:
            print(f'[Poller] feishu {self.bot_uuid}: app_id/app_secret required', flush=True)
            return
        try:
            handler = self._build_handler()
            self._ws_client = self._lark.ws.Client(
                self._app_id, self._app_secret, event_handler=handler,
                # lark_oapi 1.7.x LogLevel 枚举成员是 WARNING(非 WARN——
                # 写 WARN 会 AttributeError('WARN') 导致 WS 启动即崩)。
                log_level=self._lark.LogLevel.WARNING,
            )
            print(f'[Poller] feishu {self.bot_uuid} ws started', flush=True)
            self._ws_client.start()  # blocking; reconnects internally
        except Exception as exc:
            print(f'[Poller] feishu {self.bot_uuid} ws error: {exc}', flush=True)
        finally:
            print(f'[Poller] feishu {self.bot_uuid} ws exited', flush=True)

    def send_text(self, target, text, client_id=''):
        if not self._app_id or not self._app_secret:
            raise RuntimeError(f'feishu {self.bot_uuid} not configured')
        # 出站走独立 API client(WS 通道只收不发的官方用法): 回复目标 =
        # chat_id(p2p/group 统一)。
        self._send_content_message(target, 'text', {'text': text})

    def _send_content_message(self, target, msg_type, content):
        """飞书消息发送通用(出站 API client)。content 为消息 content JSON dict。"""
        if not self._app_id or not self._app_secret:
            raise RuntimeError(f'feishu {self.bot_uuid} not configured')
        v1 = self._lark.api.im.v1
        api = self._ensure_api_client()
        request = v1.CreateMessageRequest.builder() \
            .receive_id_type('chat_id') \
            .request_body(v1.CreateMessageRequestBody.builder()
                          .receive_id(target)
                          .msg_type(msg_type)
                          .content(json.dumps(content, ensure_ascii=False))
                          .build()) \
            .build()
        resp = api.im.v1.message.create(request)
        if not resp.success():
            raise RuntimeError(f'feishu send failed: code={resp.code} msg={resp.msg}')

    # -- 出站媒体统一接口(IM_MEDIA_ARCHITECTURE §5.1 A1 / §3.2) -----------
    # 上传: 图片 im/v1/images、文件/视频 im/v1/files 拿 key → 消息 content。
    def _upload_image(self, file_path):
        v1 = self._lark.api.im.v1
        api = self._ensure_api_client()
        with open(file_path, 'rb') as f:
            req = (v1.CreateImageRequest.builder()
                   .file(f)
                   .request_body(v1.CreateImageRequestBody.builder()
                                 .image_type('message').build())
                   .build())
            resp = api.im.v1.image.create(req)
        if not resp.success():
            raise RuntimeError(f'feishu image upload failed: code={resp.code} msg={resp.msg}')
        image_key = str(getattr(getattr(resp, 'data', None), 'image_key', '') or '')
        if not image_key:
            raise RuntimeError('feishu image upload returned empty image_key')
        return image_key

    def _upload_file(self, file_path, file_name):
        v1 = self._lark.api.im.v1
        api = self._ensure_api_client()
        with open(file_path, 'rb') as f:
            req = (v1.CreateFileRequest.builder()
                   .file(f)
                   .request_body(v1.CreateFileRequestBody.builder()
                                 .file_type('stream')
                                 .file_name(file_name or os.path.basename(file_path))
                                 .build())
                   .build())
            resp = api.im.v1.file.create(req)
        if not resp.success():
            raise RuntimeError(f'feishu file upload failed: code={resp.code} msg={resp.msg}')
        file_key = str(getattr(getattr(resp, 'data', None), 'file_key', '') or '')
        if not file_key:
            raise RuntimeError('feishu file upload returned empty file_key')
        return file_key

    def _send_image(self, target, file_path, file_name='', client_id=''):
        # 飞书官方图片 ≤10MB(open.feishu.cn im-v1 image): 全局 media_size_limit
        # (20MiB)按渠道偏宽, 超 10MB 的图会在飞书侧被拒 → 交付侧先适配
        # (2026-08-14 独立审查 A2 补齐, 企微/钉钉/微信已有同款)。
        # 格式不转换(飞书格式限制宽, allowed_formats=None); 动图保留。
        adapted = fit_image_for_upload(file_path, max_bytes=10 * 1024 * 1024, animated_ok=True)
        try:
            path = str(adapted) if adapted else file_path
            self._send_content_message(target, 'image', {'image_key': self._upload_image(path)})
        finally:
            if adapted:
                try:
                    os.remove(str(adapted))
                except OSError:
                    pass

    def _send_file(self, target, file_path, file_name='', client_id=''):
        self._send_content_message(target, 'file', {'file_key': self._upload_file(file_path, file_name)})

    def _send_video(self, target, file_path, file_name='', client_id=''):
        # 飞书视频消息 = msg_type 'media'(content file_key 同上传, 待真实凭据实测)。
        self._send_content_message(target, 'media', {'file_key': self._upload_file(file_path, file_name)})

    # -- IM 流式输出: 消息编辑打字机 ----------------------------------------
    # 语义(IM_STREAMING_DELIVERY §4.3): open 发占位消息取 message_id;
    # append 累积文本后 PUT /im/v1/messages/:id 全量替换(飞书编辑是整条
    # 更新, 不是增量); commit 最后一次更新(最终文本已在 append 累积);
    # abort 改写"生成中断"提示(IM 消息不可撤回, 至少告知用户)。
    # 所有 SDK 调用过 5 QPS 令牌桶, 超时抛错 → Go 侧 abort → delivery 兜底。

    def _ensure_api_client(self):
        if self._api_client is None:
            self._api_client = self._lark.Client.builder()\
                .app_id(self._app_id)\
                .app_secret(self._app_secret)\
                .build()
        return self._api_client

    def _update_message(self, message_id, text):
        v1 = self._lark.api.im.v1
        api = self._ensure_api_client()
        request = v1.UpdateMessageRequest.builder() \
            .message_id(message_id) \
            .request_body(v1.UpdateMessageRequestBody.builder()
                          .msg_type('text')
                          .content(json.dumps({'text': text}, ensure_ascii=False))
                          .build()) \
            .build()
        resp = api.im.v1.message.update(request)
        if not resp.success():
            raise RuntimeError(f'feishu stream update failed: code={resp.code} msg={resp.msg}')

    def send_stream_open(self, target, text=''):
        if not self._app_id or not self._app_secret:
            raise RuntimeError(f'feishu {self.bot_uuid} not configured')
        v1 = self._lark.api.im.v1
        api = self._ensure_api_client()
        request = v1.CreateMessageRequest.builder() \
            .receive_id_type('chat_id') \
            .request_body(v1.CreateMessageRequestBody.builder()
                          .receive_id(target)
                          .msg_type('text')
                          .content(json.dumps({'text': text or '…'}, ensure_ascii=False))
                          .build()) \
            .build()
        resp = api.im.v1.message.create(request)
        if not resp.success():
            raise RuntimeError(f'feishu stream open failed: code={resp.code} msg={resp.msg}')
        message_id = getattr(getattr(resp, 'data', None), 'message_id', '') or ''
        if not message_id:
            raise RuntimeError('feishu stream open: empty message_id')
        stream_id = uuid.uuid4().hex
        # 累积基线存 ''(非 '…'): 飞书编辑是全量替换(append PUT 整条), 占位符
        # 会被后续帧覆盖, 不会残留进终态——与 QQ 的"基准=首帧实际下发内容"
        # 恰好相反, 差异来自渠道契约(QQ replace 前缀严格, 飞书无前缀约束)。
        with self._stream_lock:
            self._streams[stream_id] = {
                'target': target, 'message_id': message_id,
                'text': text or '', 'edits': 0,
            }
        return stream_id

    def send_stream_append(self, stream_id, text):
        with self._stream_lock:
            st = self._streams.get(stream_id)
            if st is None:
                raise KeyError(f'feishu unknown stream {stream_id}')
            st['text'] += text
            st['edits'] += 1
            if st['edits'] > self._MAX_STREAM_EDITS:
                # 20 次编辑上限保护(官方): 停止 PUT, 文本累积到 commit
                # 最后一次更新一次性送达。
                return
            message_id = st['message_id']
        if not self._throttle.acquire():
            raise RuntimeError('feishu stream rate limited (5 QPS)')
        self._update_message(message_id, st['text'])

    def send_stream_commit(self, stream_id, text=''):
        with self._stream_lock:
            st = self._streams.pop(stream_id, None)
            if st is None:
                return  # 已结束/未知: 幂等
            if text:
                st['text'] = text
            message_id = st['message_id']
            final_text = st['text']
        if not self._throttle.acquire():
            raise RuntimeError('feishu stream rate limited (5 QPS)')
        # 最后一次更新: 保证消息为最终累积文本(编辑时限内)。
        self._update_message(message_id, final_text)

    def send_stream_abort(self, stream_id):
        with self._stream_lock:
            st = self._streams.pop(stream_id, None)
            if st is None:
                return  # 幂等
            message_id = st['message_id']
        if not self._throttle.acquire():
            return  # 限流时放弃改写(终态 delivery 仍会补发结果)
        try:
            self._update_message(message_id, '⚠️ 生成中断，请稍后查看最终结果')
        except Exception:
            pass  # 改写失败可接受: 占位消息残留, 终态 delivery 兜底

class DingTalkAdapter(BotAdapter):
    """钉钉开放平台应用(dingtalk-stream 长连接)。

    config: {app_id→app_key, app_secret}。平台只推送 @ 机器人的群消息
    (硬规则, IM_CHANNEL_BINDING §2); conversation_id = conversationId
    (群/单聊统一), 回复目标 = conversation_id。
    """

    channel_type = CHANNEL_DINGTALK

    def __init__(self, bot_uuid, config, webhook_url, *, media_root=None, webhook_secret='',
                 coalesce_window_provider=None):
        super().__init__(bot_uuid, webhook_url, media_root=media_root, webhook_secret=webhook_secret,
                         coalesce_window_provider=coalesce_window_provider)
        self._app_key = (config or {}).get('app_id') or ''  # API 层 app_id 即钉钉 app_key
        self._app_secret = (config or {}).get('app_secret') or ''
        self._client = None
        self._sdk_error = None
        try:
            import dingtalk_stream  # 惰性导入: SDK 缺失只影响本渠道
            self._dingtalk_stream = dingtalk_stream
        except Exception as exc:  # pragma: no cover
            self._sdk_error = exc

    def _run(self):
        if self._sdk_error is not None:
            print(f'[Poller] dingtalk {self.bot_uuid}: dingtalk_stream unavailable: {self._sdk_error}', flush=True)
            return
        if not self._app_key or not self._app_secret:
            print(f'[Poller] dingtalk {self.bot_uuid}: app_key/app_secret required', flush=True)
            return
        dt = self._dingtalk_stream
        from dingtalk_stream.chatbot import ChatbotMessage

        class _Handler(dt.CallbackHandler):
            def __init__(self, adapter):
                super().__init__()
                self._adapter = adapter

            async def process(self, message):
                try:
                    chatbot_msg = ChatbotMessage.from_dict(message.data)
                    self._adapter._handle_chatbot_message(chatbot_msg, message.data)
                except Exception as exc:
                    print(f'[Poller] dingtalk {self._adapter.bot_uuid} callback error: {exc}', flush=True)
                return dt.AckMessage.STATUS_OK, 'OK'

        try:
            self._client = dt.DingTalkStreamClient(dt.Credential(self._app_key, self._app_secret))
            self._client.register_callback_handler(ChatbotMessage.TOPIC, _Handler(self))
            print(f'[Poller] dingtalk {self.bot_uuid} stream started', flush=True)
            asyncio.run(self._client.start())  # blocking; 重连由 SDK 处理
        except Exception as exc:
            print(f'[Poller] dingtalk {self.bot_uuid} stream error: {exc}', flush=True)

    def _handle_chatbot_message(self, chatbot_msg, raw_data=None):
        """钉钉入站消息 → webhook body。conversation_id = conversationId
        (群/单聊统一), 平台只推 @ 消息(硬规则, 无需本侧过滤)。

        媒体提取(IM_MEDIA_ARCHITECTURE §3.1): picture/file/video/audio
        消息 content 带 downloadCode(官方: 临时下载码), 经
        POST /v1.0/robot/messageFiles/download 换文件字节; 语音消息带
        官方 ASR 文本 recognition, 直接注入 prompt(审查 S2)。"""
        text = getattr(getattr(chatbot_msg, 'text', None), 'content', '') or ''
        text = text.strip()
        media_paths, media_items = [], []
        raw = raw_data if isinstance(raw_data, dict) else {}
        msgtype = str(raw.get('msgtype') or '')
        content = raw.get('content')
        if isinstance(content, str):
            try:
                content = json.loads(content)
            except (ValueError, TypeError):
                content = {}
        if isinstance(content, dict) and self._media_dir:
            try:
                media_paths, media_items, asr_text = self._extract_media(
                    msgtype, content, str(getattr(chatbot_msg, 'conversation_id', '') or ''))
                if asr_text and not text:
                    # 语音官方 ASR: 免费转写文本直接进 prompt(零成本高价值)。
                    text = f'[语音消息转写] {asr_text}'
            except Exception as exc:
                print(f'[Poller] dingtalk media dl err ({self.bot_uuid}): {exc}', flush=True)
        if not text and not media_paths:
            return
        self.deliver_inbound([self.webhook_body(
            channel_account_id=str(
                getattr(chatbot_msg, 'sender_staff_id', None)
                or getattr(chatbot_msg, 'sender_id', None) or ''),
            conversation_id=str(getattr(chatbot_msg, 'conversation_id', '') or ''),
            message_id=str(getattr(chatbot_msg, 'message_id', '') or ''),
            text=text,
            media_paths=media_paths,
            media_items=media_items,
            # 钉钉会话形态: conversation_type=='2' = 群(IM_CHANNEL_BINDING §2)。
            conversation_type=('group'
                               if str(getattr(chatbot_msg, 'conversation_type', '') or '') == '2'
                               else 'private'),
        )])

    def _extract_media(self, msgtype, content, conversation_id):
        """钉钉 downloadCode → 文件字节 → 落盘。返回 (media_paths, media_items, asr_text)。

        官方(open.dingtalk.com/document/orgapp/download-the-file-content-of-the-
        robot-receiving-message): 图片 picture / 文件 file(fileName) / 视频
        video(videoType) / 语音 audio(recognition=官方 ASR)。端点:
        POST /v1.0/robot/messageFiles/download
        body {robotCode, downloadCode, openConversationId} → 二进制流。
        """
        download_code = str(content.get('downloadCode') or '')
        if not download_code:
            return [], [], ''
        file_name = str(content.get('fileName') or '')
        ext = {'picture': '', 'video': '.mp4'}.get(msgtype, '')
        asr_text = str(content.get('recognition') or '')
        import requests as _requests
        token = self._access_token()
        if not token:
            raise RuntimeError('dingtalk media download: access token failed')
        resp = _requests.post(
            'https://api.dingtalk.com/v1.0/robot/messageFiles/download',
            json={'robotCode': self._app_key,
                  'downloadCode': download_code,
                  'openConversationId': conversation_id},
            headers={'x-acs-dingtalk-access-token': token},
            timeout=60,
        )
        if resp.status_code != 200:
            raise RuntimeError(
                f'dingtalk media download HTTP {resp.status_code}: {resp.text[:200]}')
        data = resp.content
        if not data:
            raise RuntimeError('dingtalk media download returned empty body')
        path = media_dl.save_bytes_bounded(
            data, self._media_dir, file_name=file_name, ext=ext)
        return [path], [media_dl.build_media_item(path, self._media_root, file_name)], asr_text

    def send_text(self, target, text, client_id=''):
        # 回复: 直接调用机器人开放 API(target = conversationId, 群/单聊统一)。
        self._send_via_api(target, text)

    def _send_via_api(self, conversation_id, text):
        self._send_via_api_key(conversation_id, 'sampleText', {'content': text})

    def _send_via_api_key(self, conversation_id, msg_key, msg_param):
        """钉钉机器人发送通用(v1.0 robot/groupMessages/send, 群/单聊统一
        target=conversationId)。msg_param 为消息参数 dict(内部 JSON 序列化)。"""
        import requests
        token = self._access_token()
        if not token:
            raise RuntimeError('dingtalk access token failed')
        headers = {'x-acs-dingtalk-access-token': token}
        url = 'https://api.dingtalk.com/v1.0/robot/groupMessages/send'
        payload = {
            'robotCode': self._app_key,
            'openConversationId': conversation_id,
            'msgKey': msg_key,
            'msgParam': json.dumps(msg_param, ensure_ascii=False),
        }
        resp = requests.post(url, json=payload, headers=headers, timeout=20)
        if resp.status_code != 200:
            raise RuntimeError(f'dingtalk send HTTP {resp.status_code}: {resp.text[:200]}')
        body = resp.json() if 'json' in resp.headers.get('content-type', '') else {}
        if body.get('errcode') not in (None, 0):
            raise RuntimeError(f'dingtalk send errcode={body.get("errcode")}')

    def _upload_media(self, file_path, media_type):
        """钉钉媒体上传(oapi/media/upload, 企业内部应用) → media_id。"""
        import requests
        token = self._access_token()
        if not token:
            raise RuntimeError('dingtalk access token failed')
        with open(file_path, 'rb') as f:
            resp = requests.post(
                'https://oapi.dingtalk.com/media/upload',
                params={'access_token': token, 'type': media_type},
                files={'media': f}, timeout=60)
        if resp.status_code != 200:
            raise RuntimeError(f'dingtalk media upload HTTP {resp.status_code}: {resp.text[:200]}')
        data = resp.json()
        if data.get('errcode') not in (None, 0):
            raise RuntimeError(f'dingtalk media upload errcode={data.get("errcode")}')
        media_id = str(data.get('media_id') or '')
        if not media_id:
            raise RuntimeError('dingtalk media upload returned empty media_id')
        return media_id

    # -- 出站媒体统一接口(IM_MEDIA_ARCHITECTURE §5.1 A1 / §3.2) -----------
    #: 钉钉官方图片限制: jpg/gif/png/bmp, ≤20MB(open.dingtalk.com 上传媒体
    # 文件)。webp 不在白名单 → 交付侧转 JPEG; 动图(gif)保留。
    _DINGTALK_IMAGE_ALLOWED = {'JPEG', 'PNG', 'GIF', 'BMP'}
    _DINGTALK_IMAGE_MAX_BYTES = 20 * 1024 * 1024

    def _send_image(self, target, file_path, file_name='', client_id=''):
        # sampleImageMsg photo=media_id(机器人图片消息)。
        adapted = fit_image_for_upload(
            file_path, max_bytes=self._DINGTALK_IMAGE_MAX_BYTES,
            allowed_formats=self._DINGTALK_IMAGE_ALLOWED, animated_ok=True)
        try:
            path = str(adapted) if adapted else file_path
            self._send_via_api_key(target, 'sampleImageMsg', {'photo': self._upload_media(path, 'image')})
        finally:
            if adapted:
                try:
                    os.remove(str(adapted))
                except OSError:
                    pass

    def _send_file(self, target, file_path, file_name='', client_id=''):
        # 钉钉机器人文件消息 = sampleFileMsg{fileName,fileType,fileSize,downloadCode},
        # downloadCode 需上传到钉钉存储(端点未确认, 审查 S4 同款待实测)——
        # 先 fail-closed 走既有 delivery 错误路径, 不静默降级(决策 A3)。
        raise NotImplementedError('dingtalk file send requires downloadCode upload flow (待实测)')

    def _send_video(self, target, file_path, file_name='', client_id=''):
        # 视频 = sampleVideoMsg{downloadCode...}, 同上待实测。
        raise NotImplementedError('dingtalk video send requires downloadCode upload flow (待实测)')

    def _access_token(self):
        import requests
        resp = requests.post(
            'https://api.dingtalk.com/v1.0/oauth2/accessToken',
            json={'appKey': self._app_key, 'appSecret': self._app_secret},
            timeout=20,
        )
        if resp.status_code != 200:
            return None
        data = resp.json()
        return data.get('accessToken')


class QQAdapter(BotAdapter):
    """QQ 开放平台机器人(botpy WebSocket)。

    config: {app_id, app_secret}。订阅 C2C 与 GROUP_AT_MESSAGE(平台只推
    @ 消息, 硬规则); conversation_id: 群=group_openid / C2C=openid。
    回复目标 = conversation_id。

    流式(IM_STREAMING_DELIVERY §4.3): 仅 C2C 单聊——官方"发送单聊消息/
    流式消息"接口(POST /v2/users/{openid}/messages + stream 参数:
    state 1=生成中/10=结束, id 首条 null 后续用返回 id, index 会话内
    递增, reset 终帧全量替换); 群消息官方明确不支持流式参数(群聊由
    platform 转发判定收敛, 只发最终结果)。参数细节以真实凭据实测为准
    (linux.do 2026-03 实测 + easybot SDK 参考, index/msg_seq 递增语义
    有歧义——见 PROGRESS.md 残余风险)。
    """

    channel_type = CHANNEL_QQ
    #: 仅单聊支持原生流式接口(群聊无流式参数)。
    stream_supported = True
    #: 流式帧业务上限: 官方 SDK(openStream)不做帧数限制(仅 500ms 节流),
    #: 但平台任务可达 45 分钟, 无上限会刷爆单关系频控(单聊 20 QPM 主动)
    #: 与被动回复次数限制——设 60 帧(≈30s 活跃生成的 500ms 节流输出,
    #: 覆盖绝大多数 AI 回复)。超出后文本继续累积, 由 commit 终帧
    #: (input_state=10 全量替换)一次性送达, 内容不丢。
    _MAX_STREAM_APPENDS = 60

    def __init__(self, bot_uuid, config, webhook_url, *, media_root=None, webhook_secret='',
                 coalesce_window_provider=None):
        super().__init__(bot_uuid, webhook_url, media_root=media_root, webhook_secret=webhook_secret,
                         coalesce_window_provider=coalesce_window_provider)
        self._app_id = (config or {}).get('app_id') or ''
        self._app_secret = (config or {}).get('app_secret') or ''
        self._client = None
        self._sdk_error = None
        # 流式状态: stream_id -> {openid, msg_id, msg_seq, index, text, appends}
        self._streams = {}
        self._stream_lock = threading.Lock()
        # 入站目标类型记忆: conversation_id -> 'group' | 'c2c'。QQ 的
        # group_openid 与 C2C openid 格式上无法区分, send_text 默认先试群
        # 再退单聊——每次私聊回复都白打一次群接口(实测 botpy 刷 11255
        # invalid request 错误日志)。已知目标直接走正确接口, 未知目标保持
        # 先群后单聊兜底。
        self._target_kinds = {}
        # 记忆上限: 超限直接清空重学(最坏一次无效尝试, 防长期运行内存增长)。
        self._target_kinds_max = 512
        # 最近一条 C2C 入站消息 id(被动回复 msg_id; 无则主动消息流式)。
        self._last_c2c_msg_id = ''
        try:
            import botpy  # 惰性导入: SDK 缺失只影响本渠道
            self._botpy = botpy
        except Exception as exc:  # pragma: no cover
            self._sdk_error = exc

    def _run(self):
        if self._sdk_error is not None:
            print(f'[Poller] qq {self.bot_uuid}: botpy unavailable: {self._sdk_error}', flush=True)
            return
        if not self._app_id or not self._app_secret:
            print(f'[Poller] qq {self.bot_uuid}: app_id/app_secret required', flush=True)
            return
        # botpy 1.2.x 的 Client.__init__ 直接调 asyncio.get_event_loop()——在
        # 非主线程无当前事件循环时抛 RuntimeError, 必须先建 loop 并 set。
        # (主线程可免建; 显式 set 后子线程行为与主线程一致。)
        try:
            asyncio.get_event_loop()
        except RuntimeError:
            asyncio.set_event_loop(asyncio.new_event_loop())
        bp = self._botpy

        class _QQClient(bp.Client):
            def __init__(self, adapter):
                self._adapter = adapter
                super().__init__(intents=self._adapter._intents(), ext_handlers=False)

            async def on_ready(self):
                print(f'[Poller] qq {self._adapter.bot_uuid} ready', flush=True)

            async def on_c2c_message_create(self, message):
                self._adapter._handle_message(message, is_group=False)

            async def on_group_at_message_create(self, message):
                self._adapter._handle_message(message, is_group=True)

        self._client = _QQClient(self)
        try:
            print(f'[Poller] qq {self.bot_uuid} ws started', flush=True)
            self._client.run(appid=self._app_id, secret=self._app_secret)
        except Exception as exc:
            print(f'[Poller] qq {self.bot_uuid} ws error: {exc}', flush=True)

    @staticmethod
    def _intents():
        try:
            import botpy
            # QQ 开放平台机器人(群@ + C2C 单聊) = 公域消息 intent(1<<25), 同时
            # 驱动 on_group_at_message_create 与 on_c2c_message_create。
            # c2c_message/group_at_message 不是 botpy 1.2.x 的有效 flag 名
            # (赋值即 AttributeError, 被吞后 intents=None → Client 构造崩溃)。
            return botpy.Intents(public_messages=True)
        except Exception:
            return None

    def _handle_message(self, message, is_group):
        try:
            message_id = str(getattr(message, 'id', '') or '')
            if not message_id:
                return
            content = (getattr(message, 'content', '') or '').strip()
            author = getattr(message, 'author', None)
            member_openid = str(getattr(author, 'member_openid', '') or '')
            user_openid = str(getattr(author, 'user_openid', '') or '')
            group_openid = str(getattr(message, 'group_openid', '') or '')
            if is_group:
                conversation_id = group_openid or member_openid
                channel_account_id = member_openid or user_openid
            else:
                conversation_id = user_openid  # C2C: openid 即对话单元
                channel_account_id = user_openid
            if not is_group and message_id:
                # 流式被动回复锚点(单聊 4 次/条限制下的 msg_id+msg_seq 去重)。
                self._last_c2c_msg_id = message_id
            with self._stream_lock:
                if len(self._target_kinds) >= self._target_kinds_max:
                    self._target_kinds.clear()
                self._target_kinds[conversation_id] = 'group' if is_group else 'c2c'
            # 入站媒体提取(IM_MEDIA_ARCHITECTURE §3.1): 官方事件
            # attachments[] 直带下载 URL(带 rkey 时效)——事件到达即下载
            # (URL 过期后无法重试), 失败丢弃媒体保留文本(与微信同语义)。
            media_paths, media_items = self._extract_attachments(message)
            if not content and not media_paths:
                # 无文本且媒体提取失败(下载/落盘异常): 丢弃并记日志, 不回
                # 误导性的 "empty message ignored"(审查 B1 统一行为)。
                return
            self.deliver_inbound([self.webhook_body(
                channel_account_id=channel_account_id,
                conversation_id=conversation_id,
                message_id=message_id,
                text=content,
                media_paths=media_paths,
                media_items=media_items,
                conversation_type='group' if is_group else 'private',
            )])
        except Exception as exc:
            print(f'[Poller] qq {self.bot_uuid} handle error: {exc}', flush=True)

    def _extract_attachments(self, message):
        """QQ 入站附件提取: attachments[].url 直下(host 白名单 + 大小上限)。

        返回 (media_paths, media_items); 单条失败丢弃该条(日志), 不阻塞
        文本消息——与微信 download_media 失败语义一致(IM_MEDIA_ARCH §11)。
        """
        media_paths, media_items = [], []
        if not self._media_dir:
            return media_paths, media_items
        for att in (getattr(message, 'attachments', None) or []):
            url = str(getattr(att, 'url', '') or '')
            if not url:
                continue
            file_name = str(getattr(att, 'filename', '') or '')
            content_type = str(getattr(att, 'content_type', '') or '') or None
            try:
                path = media_dl.download_url_bounded(
                    url, self._media_dir, file_name=file_name,
                    allowed_hosts=media_dl.QQ_MEDIA_HOSTS)
                if path:
                    media_paths.append(path)
                    media_items.append(media_dl.build_media_item(
                        path, self._media_root, file_name, content_type))
            except Exception as exc:
                print(f'[Poller] qq media dl err ({self.bot_uuid}): {exc}', flush=True)
        return media_paths, media_items

    def send_text(self, target, text, client_id=''):
        if self._client is None:
            raise RuntimeError(f'qq {self.bot_uuid} not connected')
        # 群回复 target=group_openid, C2C 回复 target=openid——由入站消息的
        # conversation_id 决定(先试群再退 C2C)。botpy 出站是异步 API,
        # 调度到 bot 自身事件循环。
        async def _send():
            with self._stream_lock:
                kind = self._target_kinds.get(target)
            if kind == 'group':
                await self._client.api.post_group_message(
                    group_openid=target, msg_type=2,
                    markdown={'content': text[:2000]},
                )
                return
            if kind == 'c2c':
                await self._client.api.post_c2c_message(
                    openid=target, msg_type=0, content=text[:2000],
                )
                return
            # 未知目标(如主动发起/历史目标): 先群后单聊兜底。
            try:
                await self._client.api.post_group_message(
                    group_openid=target, msg_type=2,
                    markdown={'content': text[:2000]},
                )
                return
            except Exception:
                pass
            await self._client.api.post_c2c_message(
                openid=target, msg_type=0, content=text[:2000],
            )

        loop = getattr(self._client, 'loop', None)
        if loop is None or loop.is_closed():
            raise RuntimeError(f'qq {self.bot_uuid} event loop unavailable')
        fut = asyncio.run_coroutine_threadsafe(_send(), loop)
        fut.result(timeout=30)

    # -- 出站媒体统一接口(IM_MEDIA_ARCHITECTURE §5.1 A1 / §3.2) -----------
    # QQ 富媒体: 先上传拿 file_info(官方: 整文件上传需公网 URL 不可用→分片;
    # 单聊/群聊上传不互通; file_info 有 TTL 故在发送时刻上传——审查 B2)。
    # 异步 delivery 必超被动窗口(msg_id 5 分钟有效, 错误码 304103)→ 主动
    # 消息路径(审查 B5), 消耗主动频次(单聊 20 QPM/单关系)——per-target
    # 令牌桶预算, 超限 fail-closed 走 delivery 重试。

    #: 主动消息频控: 单聊 20 QPM(官方 overview 渠道矩阵)。留 25% 余量。
    _QQ_ACTIVE_QPM = 15
    _qq_send_buckets = {}  # target -> _TokenBucket(每关系独立预算)
    _qq_buckets_max = 512

    def _qq_bucket(self, target):
        with self._stream_lock:
            if len(self._qq_send_buckets) >= self._qq_buckets_max:
                self._qq_send_buckets.clear()
            bucket = self._qq_send_buckets.get(target)
            if bucket is None:
                bucket = _TokenBucket(capacity=self._QQ_ACTIVE_QPM,
                                      refill_per_sec=self._QQ_ACTIVE_QPM / 60.0)
                self._qq_send_buckets[target] = bucket
            return bucket

    def _qq_request(self, method, route_template, target, payload=None):
        """调度 botpy 内部 _http.request 到事件循环(Route 模式, 与
        _send_stream_frame 同构)。route_template 含 {openid} 或
        {group_openid} 占位。限流(429/50002)抛 _QQRateLimited。"""
        async def _call():
            from botpy.http import Route
            route = Route(method, route_template, openid=target, group_openid=target)
            if payload is not None:
                return await self._client.api._http.request(route, json=payload)
            return await self._client.api._http.request(route)
        loop = getattr(self._client, 'loop', None)
        if loop is None or loop.is_closed():
            raise RuntimeError(f'qq {self.bot_uuid} event loop unavailable')
        fut = asyncio.run_coroutine_threadsafe(_call(), loop)
        resp = fut.result(timeout=120)
        if isinstance(resp, dict) and _is_qq_rate_limit(resp):
            raise _QQRateLimited(resp)
        return resp or {}

    def _media_endpoint_sets(self, target):
        """按目标类型返回 QQ 富媒体端点组(官方: 单聊/群聊上传不互通)。

        每组 = (prepare, part_finish, files, messages) 四端点模板。已知目标
        只返回对应组; 未知目标先群后单聊兜底(send_text 同款语义, 审查 I3)——
        群目标打 C2C 端点或反之都会失败, 兜底保证 poller 重启/多实例后
        媒体交付仍可用。上传与发送必须同组(单聊上传的 file_info 只能发
        单聊, 官方 rich-media 文档)。
        """
        with self._stream_lock:
            kind = self._target_kinds.get(target)
        group = (
            '/v2/groups/{group_openid}/upload_prepare',
            '/v2/groups/{group_openid}/upload_part_finish',
            '/v2/groups/{group_openid}/files',
            '/v2/groups/{group_openid}/messages',
        )
        c2c = (
            '/v2/users/{openid}/upload_prepare',
            '/v2/users/{openid}/upload_part_finish',
            '/v2/users/{openid}/files',
            '/v2/users/{openid}/messages',
        )
        if kind == 'group':
            return [group]
        if kind == 'c2c':
            return [c2c]
        return [group, c2c]

    def _upload_media(self, target, file_path, file_name, file_type):
        """QQ 分片上传 → (file_info, send_tpl)(官方 4 步流程, 2026-08-14 审查 B2)。

        官方(rich-media 文档): ① upload_prepare 拿 upload_id/block_size/各片
        presigned_url → ② 逐片 PUT → ③ 每片 PUT 成功后 upload_part_finish
        (upload_id/part_index/block_size/md5) → ④ 全部分片完成后 POST
        .../files {upload_id, file_type, file_name, srv_send_msg=false}
        合并 → file_info。

        md5/sha1/md5_10m 单遍流式计算(内存峰值 = 单分片缓冲, 审查 I4);
        md5_10m 官方明示为文件前 10002432 字节(约 9.54MB)的 MD5。
        upload_prepare 10 QPS、part_finish 10 QPS——串行天然满足, 不加桶。
        file_info 有 TTL, 必须在发送时刻上传(设计 B2)。
        """
        import hashlib as _hl
        size = os.path.getsize(file_path)
        md5 = _hl.md5()
        sha1 = _hl.sha1()
        md5_10m = _hl.md5()
        _md5_10m_remaining = 10002432
        with open(file_path, 'rb') as f:
            while True:
                chunk = f.read(64 * 1024)
                if not chunk:
                    break
                md5.update(chunk)
                sha1.update(chunk)
                if _md5_10m_remaining > 0:
                    take = min(_md5_10m_remaining, len(chunk))
                    md5_10m.update(chunk[:take])
                    _md5_10m_remaining -= take
        last_exc = None
        for prepare_tpl, part_finish_tpl, files_tpl, send_tpl in self._media_endpoint_sets(target):
            try:
                prepare = self._qq_request('POST', prepare_tpl, target, {
                    'file_type': file_type, 'file_size': str(size),
                    'file_name': file_name or os.path.basename(file_path),
                    'md5': md5.hexdigest(), 'sha1': sha1.hexdigest(),
                    'md5_10m': md5_10m.hexdigest(),
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
                    # 切片: 偏移按响应级 block_size 递增(index 从 0 起, 官方);
                    # 单片长度优先逐片 block_size(官方 parts[].block_size)。
                    part_block = int(part.get('block_size') or 0) or block_size
                    offset = idx * block_size
                    length = min(part_block, max(0, size - offset))
                    if length <= 0:
                        raise RuntimeError(f'qq upload part {idx} offset out of range')
                    part_md5 = _hl.md5()
                    with open(file_path, 'rb') as f:
                        f.seek(offset)
                        data = f.read(length)
                    part_md5.update(data)
                    self._put_part_with_retry(presigned, data, idx)
                    # ③ 每片完成确认(官方: PUT 成功后必须通知, 否则合并失败)。
                    self._qq_request('POST', part_finish_tpl, target, {
                        'upload_id': upload_id, 'part_index': idx,
                        'block_size': str(length), 'md5': part_md5.hexdigest(),
                    })
                # ④ 合并: .../files {upload_id, file_type, file_name, srv_send_msg}
                # (srv_send_msg=false 仅上传不直发, 发送走 _send_media_message
                # 统一频控; 官方上传接口频率限制 50 QPS)。
                finish = self._qq_request('POST', files_tpl, target, {
                    'upload_id': upload_id,
                    'file_type': file_type,
                    'file_name': file_name or os.path.basename(file_path),
                    'srv_send_msg': False,
                })
                file_info = str(finish.get('file_info') or '')
                if not file_info:
                    raise RuntimeError('qq upload merge returned empty file_info')
                return file_info, send_tpl
            except _QQRateLimited:
                raise  # 限流是频控信号, 换端点无意义(fail-closed)
            except Exception as exc:
                # 未知目标端点不匹配(如群目标打 C2C 端点): 尝试下一组;
                # 已知目标只有一组, 异常原样上抛不掩盖。
                last_exc = exc
        raise RuntimeError(f'qq media upload failed: {last_exc}')

    @staticmethod
    def _put_part_with_retry(presigned, data, part_idx):
        """分片 PUT 到预签名 URL(官方 upload_config.retry_timeout=300s 语义的
        有界版): 最多 3 次退避重试, 仍失败抛错(整体上传失败 → delivery 重试)。
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

    def _send_media_message(self, target, file_info, send_tpl):
        """主动媒体消息(msg_type=7 media.file_info, 官方)。频控: per-target
        令牌桶, 超限 fail-closed(抛错 → Go delivery 重试)。send_tpl 必须与
        上传同场景(单聊/群聊 file_info 不互通)。"""
        if not self._qq_bucket(target).acquire(timeout=5.0):
            raise RuntimeError(f'qq active media rate limited for {target} (15 QPM)')
        self._qq_request('POST', send_tpl, target,
                         {'msg_type': 7, 'media': {'file_info': file_info}})

    def _send_image(self, target, file_path, file_name='', client_id=''):
        file_info, send_tpl = self._upload_media(target, file_path, file_name, file_type=1)
        self._send_media_message(target, file_info, send_tpl)

    def _send_file(self, target, file_path, file_name='', client_id=''):
        file_info, send_tpl = self._upload_media(target, file_path, file_name, file_type=4)
        self._send_media_message(target, file_info, send_tpl)

    def _send_video(self, target, file_path, file_name='', client_id=''):
        file_info, send_tpl = self._upload_media(target, file_path, file_name, file_type=2)
        self._send_media_message(target, file_info, send_tpl)

    # -- IM 流式输出: 单聊原生流式接口 -------------------------------------
    # 语义(IM_STREAMING_DELIVERY §4.3): open 发首帧(stream.id=null)拿
    # 流式消息 id; append 续接(stream.id=返回 id, state=1 生成中, 内容
    # 全量替换语义——与飞书编辑同, 发送累积文本); commit 终帧(state=10,
    # reset=true, 全量最终文本); abort 终帧 + 中断提示(QQ 无编辑, 终帧
    # 结束"生成中"状态, 最终结果由 delivery 补发)。群聊拒绝(官方无流式
    # 参数)。botpy 当前版本无流式封装, 走其内部 _http.request(Route 与
    # api.py 自身 post_c2c_message 实现一致); 版本升级后如官方封装流式
    # API 再切换。

    def _send_stream_frame(self, openid, text, *, state, index, msg_id, msg_seq,
                           stream_msg_id='', timeout=30):
        """发一帧官方 stream_messages 帧(调度到 bot 事件循环)。返回响应 dict。

        官方契约(@tencent-connect/qqbot-nodejs 1.0.4 实证, 与 StreamReq proto
        一致): POST /v2/users/{openid}/stream_messages, 扁平字段——
        input_mode='replace'(全量替换)、input_state 1=生成中/10=结束、
        content_type='markdown'、content_raw=全量文本、event_id/msg_id=
        被动回复锚点、stream_msg_id=首帧响应 id 后续帧携带、msg_seq=同一
        流所有帧共享(仅 index 递增)、index=从 0 起每帧递增。
        """
        async def _send():
            from botpy.http import Route  # botpy 内部(与 api.py 同源)
            payload = {
                'input_mode': 'replace',
                'input_state': state,          # 1=GENERATING, 10=DONE
                'content_type': 'markdown',
                'content_raw': text,
                'event_id': msg_id,
                'msg_id': msg_id,
                'msg_seq': msg_seq,
                'index': index,
            }
            if stream_msg_id:
                payload['stream_msg_id'] = stream_msg_id
            route = Route('POST', '/v2/users/{openid}/stream_messages', openid=openid)
            # 限流重试(官方 SDK sendWithRetry 同构): 429/50002 指数退避
            # 1s/2s 共 2 次重试; 重试帧 index 递增(官方同款, 避免 stale
            # index 冲突)。仍失败抛给上层(append 失败由 commit 兜底)。
            for attempt in range(3):
                try:
                    resp = await self._client.api._http.request(route, json=payload)
                    if isinstance(resp, dict) and _is_qq_rate_limit(resp):
                        raise _QQRateLimited(resp)
                    print(f'[Poller] qq stream frame ok state={state} index={index} len={len(text)} sid={stream_msg_id or "-"} resp_id={(resp or {}).get("id", "-") if isinstance(resp, dict) else "-"}', flush=True)
                    return resp or {}
                except _QQRateLimited:
                    if attempt >= 2:
                        raise
                    payload['index'] = payload['index'] + 1
                    await asyncio.sleep(2 ** attempt)
                except Exception:
                    if attempt >= 2:
                        raise
                    payload['index'] = payload['index'] + 1
                    await asyncio.sleep(2 ** attempt)

        loop = getattr(self._client, 'loop', None)
        if loop is None or loop.is_closed():
            raise RuntimeError(f'qq {self.bot_uuid} event loop unavailable')
        fut = asyncio.run_coroutine_threadsafe(_send(), loop)
        return fut.result(timeout=timeout)

    def send_stream_open(self, target, text=''):
        """target = C2C openid。发首帧(无 stream_msg_id), 响应 id 即流句柄。

        官方 SDK 要求流式必须带入站 msg_id(被动回复形态)——无锚点直接
        拒绝(主动消息不可流式)。"""
        if self._client is None:
            raise RuntimeError(f'qq {self.bot_uuid} not connected')
        if not target:
            raise ValueError('qq stream open requires C2C openid target')
        msg_id = self._last_c2c_msg_id or ''
        if not msg_id:
            raise RuntimeError('qq stream open requires an inbound c2c msg_id anchor')
        # msg_seq 同一流共享(官方 SDK getNextMsgSeq 同构: 时间戳^随机 % 65536)。
        msg_seq = (int(time.time() * 1000) % 100_000_000 ^ __import__('random').randrange(65536)) % 65536
        resp = self._send_stream_frame(
            target, text or '…', state=1, index=0, msg_id=msg_id, msg_seq=msg_seq)
        stream_id = (resp or {}).get('id') or ''
        if not stream_id:
            raise RuntimeError('qq stream open: empty stream id in response')
        # 首帧内容 = text or '…'——累积基准必须与实际下发内容一致: QQ replace
        # 模式每帧须以已下发前缀开头(官方契约), 基准漂移即 40007。正常路径
        # scheduler 的 open 已携带首段文本(text 非空); '…' 仅为无文本兜底。
        with self._stream_lock:
            self._streams[stream_id] = {
                'openid': target, 'msg_id': msg_id,
                'msg_seq': msg_seq, 'index': 0,
                'text': text or '…', 'appends': 0,
            }
        return stream_id

    def send_stream_append(self, stream_id, text):
        with self._stream_lock:
            st = self._streams.get(stream_id)
            if st is None:
                raise KeyError(f'qq unknown stream {stream_id}')
            st['text'] += text
            st['appends'] += 1
            if st['appends'] > self._MAX_STREAM_APPENDS:
                # 被动回复 4 次/条上限保护: 停止追加帧, 文本累积到 commit
                # 终帧(input_state=10 全量替换)一次性送达。
                return
            st['index'] += 1
            frame = dict(st)
        try:
            self._send_stream_frame(
                frame['openid'], frame['text'],
                state=1, index=frame['index'], msg_id=frame['msg_id'],
                msg_seq=frame['msg_seq'], stream_msg_id=stream_id)
        except Exception:
            # 追加帧失败(限流/网络): 不再续帧, commit 终帧兜底。
            with self._stream_lock:
                st['appends'] = self._MAX_STREAM_APPENDS + 1

    def send_stream_commit(self, stream_id, text=''):
        with self._stream_lock:
            st = self._streams.pop(stream_id, None)
            if st is None:
                return  # 幂等
            if text:
                st['text'] = text
            index = st['index'] + 1
            frame = dict(st)
        # 终帧: input_state=10(DONE) 全量最终文本。
        self._send_stream_frame(
            frame['openid'], frame['text'],
            state=10, index=index, msg_id=frame['msg_id'],
            msg_seq=frame['msg_seq'], stream_msg_id=stream_id)

    def send_stream_abort(self, stream_id):
        with self._stream_lock:
            st = self._streams.pop(stream_id, None)
            if st is None:
                return  # 幂等
            index = st['index'] + 1
            frame = dict(st)
            text = frame['text'] + '\n\n⚠️ 生成中断，请稍后查看最终结果'
        try:
            # 终帧(DONE)带中断提示: 保证消息闭合(官方 cancel() 不发 DONE,
            # 消息会停在生成中状态)。
            self._send_stream_frame(
                frame['openid'], text,
                state=10, index=index, msg_id=frame['msg_id'],
                msg_seq=frame['msg_seq'], stream_msg_id=stream_id)
        except Exception:
            pass  # 终帧失败可接受: delivery 兜底补发最终结果


class WeComAdapter(BotAdapter):
    """企业微信智能机器人(wecom_aibot_sdk WebSocket)。

    config: {app_id: bot_id, app_secret: secret}(复用凭据槽位)。SDK 为纯
    asyncio 客户端: 本适配器在线程内自管事件循环(与 botpy/dingtalk-stream
    的同步 run() 封装不同)。入站 frame = {cmd, headers:{req_id}, body};
    conversation_id = chatid(单聊=userid, 群聊=群 ID——SDK 文档注释),
    conversation_type = private(chatid==userid) / group。

    出站(IM_STREAMING_DELIVERY §4.3): 平台 delivery 是异步主动推送, 不走
    被动回复窗口, 统一用 WSClient.send_message(chatid, body):
    - 终态文本: SEND_MSG 只支持 markdown/template_card/media(SDK
      SendMsgBody 无 text), 用 markdown 类型承载纯文本;
    - 流式: SEND_MSG + stream 帧(与被动 reply_stream 同协议层格式)。
      若企微服务端拒绝 SEND_MSG 流式帧, Go 侧 BeginReply 失败自动
      fail-closed 回退终态 delivery(渠道矩阵判定)。
    """

    channel_type = CHANNEL_WECOM
    #: 流式通过 SEND_MSG + stream 帧实现; 失败由平台判定矩阵收敛回终态。
    stream_supported = True

    def __init__(self, bot_uuid, config, webhook_url, *, media_root=None, webhook_secret='',
                 coalesce_window_provider=None):
        super().__init__(bot_uuid, webhook_url, media_root=media_root, webhook_secret=webhook_secret,
                         coalesce_window_provider=coalesce_window_provider)
        self._bot_id = (config or {}).get('app_id') or ''
        self._secret = (config or {}).get('app_secret') or ''
        self._client = None
        self._loop = None
        self._sdk_error = None
        # 流式状态: stream_id -> {target, text}
        self._streams = {}
        self._stream_lock = threading.Lock()
        try:
            import wecom_aibot_sdk  # 惰性导入: SDK 缺失只影响本渠道
            self._wecom_sdk = wecom_aibot_sdk
        except Exception as exc:  # pragma: no cover - 环境缺失路径
            self._sdk_error = exc

    # -- 入站 --------------------------------------------------------------
    def _handle_wecom_message(self, frame):
        """企微智能机器人入站帧 → webhook body。frame 为 SDK 消息回调
        (message_handler.py 透传的 dict: {cmd, headers, body})。"""
        body = frame.get('body', {}) if isinstance(frame, dict) else {}
        if not isinstance(body, dict):
            return
        message_id = str(body.get('msgid') or '')
        if not message_id:
            return
        sender_id = str((body.get('from') or {}).get('userid', '') or '')
        chat_id = str(body.get('chatid') or '') or sender_id
        text = ''
        media_paths, media_items = [], []
        msgtype = str(body.get('msgtype') or '')
        if msgtype == 'text':
            text = str((body.get('text') or {}).get('content', '') or '')
        elif msgtype in ('image', 'file', 'voice', 'video') and self._media_dir:
            # 入站媒体提取(IM_MEDIA_ARCHITECTURE §3.1): 媒体消息带 media_id
            # (部分类型另带直链 URL)。下载失败丢弃媒体保留文本(与微信同语义)。
            try:
                media_paths, media_items = self._extract_media(
                    msgtype, body.get(msgtype))
            except Exception as exc:
                print(f'[Poller] wecom media dl err ({self.bot_uuid}): {exc}', flush=True)
        # 单聊 chatid == 发送者 userid(SDK 文档: 单聊会话 ID = userid);
        # sender 缺失时保守归群(空==空不得判为私聊)。
        conversation_type = 'private' if sender_id and chat_id == sender_id else 'group'
        if not text and not media_paths:
            return
        self.deliver_inbound([self.webhook_body(
            channel_account_id=sender_id,
            conversation_id=chat_id,
            message_id=message_id,
            text=text,
            media_paths=media_paths,
            media_items=media_items,
            conversation_type=conversation_type,
        )])

    def _extract_media(self, msgtype, media):
        """企微入站媒体: 优先回调直链 URL(若有), 否则 media_id 走
        GET qyapi.weixin.qq.com/cgi-bin/media/get?access_token=&media_id=
        (官方 developer.work.weixin.qq.com/document/path/90254, media_id 3 天
        有效、企业内应用共享)。返回 (media_paths, media_items)。

        access_token 来源依赖智能机器人凭据(bot_id/secret, 非 corpid/secret):
        SDK 连接已换取 token 时探测复用; 未确认的 token 端点在真实凭据冒烟
        时补齐(审查 S4 同款残余风险)。"""
        if not isinstance(media, dict):
            return [], []
        media_id = str(media.get('media_id') or '')
        direct_url = str(media.get('image_url') or media.get('voice_url')
                         or media.get('video_url') or '')
        file_name = str(media.get('filename') or media.get('file_name') or '')
        if direct_url:
            path = media_dl.download_url_bounded(
                direct_url, self._media_dir, file_name=file_name,
                allowed_hosts=media_dl.WECOM_MEDIA_HOSTS)
            if path:
                return [path], [media_dl.build_media_item(
                    path, self._media_root, file_name)]
        if not media_id:
            return [], []
        token = self._client_access_token()
        if not token:
            raise RuntimeError('wecom media download: access token unavailable '
                               '(SDK token endpoint 待真实凭据实测)')
        resp = requests.get(
            'https://qyapi.weixin.qq.com/cgi-bin/media/get',
            params={'access_token': token, 'media_id': media_id},
            timeout=60,
        )
        if resp.status_code != 200:
            raise RuntimeError(
                f'wecom media get HTTP {resp.status_code}: {resp.text[:200]}')
        data = resp.content
        if not data:
            raise RuntimeError('wecom media get returned empty body')
        path = media_dl.save_bytes_bounded(data, self._media_dir, file_name=file_name)
        return [path], [media_dl.build_media_item(path, self._media_root, file_name)]

    def _client_access_token(self):
        """企微智能机器人 access_token(媒体下载用): 探测 SDK 连接后缓存的
        token(属性名随 SDK 版本); 无则返回 ''(调用方按未实现降级)。"""
        if self._client is None:
            return ''
        for attr in ('access_token', 'token', '_token', 'bot_access_token'):
            value = getattr(self._client, attr, None)
            if isinstance(value, str) and value:
                return value
        return ''

    # -- 生命周期 ----------------------------------------------------------
    def _run(self):
        if self._sdk_error is not None:
            print(f'[Poller] wecom {self.bot_uuid}: wecom_aibot_sdk unavailable: {self._sdk_error}', flush=True)
            return
        if not self._bot_id or not self._secret:
            print(f'[Poller] wecom {self.bot_uuid}: app_id/app_secret required', flush=True)
            return
        loop = asyncio.new_event_loop()
        self._loop = loop
        asyncio.set_event_loop(loop)
        try:
            loop.run_until_complete(self._async_run())
        except Exception as exc:
            print(f'[Poller] wecom {self.bot_uuid} ws error: {exc}', flush=True)
        finally:
            self._loop = None
            loop.close()
            print(f'[Poller] wecom {self.bot_uuid} ws exited', flush=True)

    async def _async_run(self):
        sdk = self._wecom_sdk
        client = sdk.WSClient(self._bot_id, self._secret)
        self._client = client

        def dispatch(event):
            def handler(frame):
                try:
                    self._handle_wecom_message(frame)
                except Exception as exc:
                    print(f'[Poller] wecom {self.bot_uuid} {event} error: {exc}', flush=True)
            return handler

        client.on('message.text', dispatch('text'))
        client.on('message.image', dispatch('image'))
        client.on('message.file', dispatch('file'))
        print(f'[Poller] wecom {self.bot_uuid} ws started', flush=True)
        await client.connect()  # blocking; reconnects internally

    # -- 出站 --------------------------------------------------------------
    def _run_coro(self, coro):
        """同步桥: 把 async 出站调用提交到连接事件循环并等待结果。"""
        if self._loop is None or self._client is None:
            raise RuntimeError(f'wecom {self.bot_uuid} not connected')
        return asyncio.run_coroutine_threadsafe(coro, self._loop).result(timeout=WEBHOOK_TIMEOUT)

    def send_text(self, target, text, client_id=''):
        if not self._bot_id or not self._secret:
            raise RuntimeError(f'wecom {self.bot_uuid} not configured')
        # SEND_MSG 无 text 类型(SDK SendMsgBody): markdown 承载纯文本。
        self._run_coro(self._client.send_message(
            target, {'msgtype': 'markdown', 'markdown': {'content': text}}))

    # -- 出站媒体统一接口(IM_MEDIA_ARCHITECTURE §5.1 A1 / §3.2) -----------
    # 上传: qyapi cgi-bin/media/upload(官方 developer.work.weixin.qq.com
    # /document/path/90253, 智能机器人同族) → media_id; 发送: SEND_MSG
    # media 类型(SDK SendMsgBody 支持 media, 帧结构待真实凭据实测)。
    def _upload_media(self, file_path, media_type):
        token = self._client_access_token()
        if not token:
            raise RuntimeError('wecom media upload: access token unavailable '
                               '(SDK token endpoint 待实测)')
        with open(file_path, 'rb') as f:
            resp = requests.post(
                'https://qyapi.weixin.qq.com/cgi-bin/media/upload',
                params={'access_token': token, 'type': media_type},
                files={'media': f}, timeout=60)
        if resp.status_code != 200:
            raise RuntimeError(f'wecom media upload HTTP {resp.status_code}: {resp.text[:200]}')
        data = resp.json()
        if data.get('errcode') not in (None, 0):
            raise RuntimeError(f'wecom media upload errcode={data.get("errcode")}')
        media_id = str(data.get('media_id') or '')
        if not media_id:
            raise RuntimeError('wecom media upload returned empty media_id')
        return media_id

    def _send_media_message(self, target, media_id):
        self._run_coro(self._client.send_message(
            target, {'msgtype': 'media', 'media': {'media_id': media_id}}))

    #: 企微官方图片限制: 仅 JPG/PNG, ≤10MB(developer.work.weixin.qq.com
    # 上传临时素材)。白名单外格式(webp/gif/bmp)与动图交付侧转 JPEG。
    _WECOM_IMAGE_ALLOWED = {'JPEG', 'PNG'}
    _WECOM_IMAGE_MAX_BYTES = 10 * 1024 * 1024

    def _send_image(self, target, file_path, file_name='', client_id=''):
        adapted = fit_image_for_upload(
            file_path, max_bytes=self._WECOM_IMAGE_MAX_BYTES,
            allowed_formats=self._WECOM_IMAGE_ALLOWED, animated_ok=False)
        try:
            path = str(adapted) if adapted else file_path
            self._send_media_message(target, self._upload_media(path, 'image'))
        finally:
            if adapted:
                try:
                    os.remove(str(adapted))
                except OSError:
                    pass

    def _send_file(self, target, file_path, file_name='', client_id=''):
        self._send_media_message(target, self._upload_media(file_path, 'file'))

    def _send_video(self, target, file_path, file_name='', client_id=''):
        self._send_media_message(target, self._upload_media(file_path, 'video'))

    # -- IM 流式输出: SEND_MSG + stream 帧 ----------------------------------
    # 语义(IM_STREAMING_DELIVERY §4.3 对齐): open 发首帧(finish=False);
    # append 累积文本后发增量帧(服务端按 stream_id 合并); commit 发终帧
    # (finish=True 全量文本); abort 发终帧 + 中断提示。未知流幂等。

    def _stream_frame(self, target, stream_id, content, finish):
        self._run_coro(self._client.send_message(target, {
            'msgtype': 'stream',
            'stream': {'id': stream_id, 'finish': finish, 'content': content},
        }))

    def send_stream_open(self, target, text=''):
        if not self._bot_id or not self._secret:
            raise RuntimeError(f'wecom {self.bot_uuid} not configured')
        stream_id = uuid.uuid4().hex
        # 累积基线存 ''(非 '…'): 企微服务端按 stream_id 合并帧(全量替换语义),
        # 首帧占位被后续帧覆盖, 不残留进终态(与 QQ replace 前缀严格契约相反)。
        with self._stream_lock:
            self._streams[stream_id] = {'target': target, 'text': text or ''}
        try:
            self._stream_frame(target, stream_id, text or '…', finish=False)
        except Exception:
            with self._stream_lock:
                self._streams.pop(stream_id, None)  # 首帧失败不留残留条目
            raise
        return stream_id

    def send_stream_append(self, stream_id, text):
        with self._stream_lock:
            st = self._streams.get(stream_id)
            if st is None:
                raise KeyError(f'wecom unknown stream {stream_id}')
            st['text'] += text
            content = st['text']
            target = st['target']
        self._stream_frame(target, stream_id, content, finish=False)

    def send_stream_commit(self, stream_id, text=''):
        with self._stream_lock:
            st = self._streams.pop(stream_id, None)
            if st is None:
                return  # 已结束/未知: 幂等
            if text:
                st['text'] = text
            content = st['text']
            target = st['target']
        self._stream_frame(target, stream_id, content, finish=True)

    def send_stream_abort(self, stream_id):
        with self._stream_lock:
            st = self._streams.pop(stream_id, None)
            if st is None:
                return  # 幂等
            content = st['text'] + '\n\n⚠️ 生成中断，请稍后查看最终结果'
            target = st['target']
        try:
            self._stream_frame(target, stream_id, content, finish=True)
        except Exception:
            pass  # 终帧失败可接受: delivery 兜底补发最终结果


class BotManager:
    """BotAdapter 注册表: bot_uuid → adapter(channel_type 工厂)。

    配置来源 = 平台热推(/start /stop), 复用既有配置热更新链路
    (IM_CHANNEL_BINDING §5): 新增/更新/解绑由 Go 控制面触发连接重载。

    media_root 留存(2026-08-13 审查 I4/D7): 入站媒体下载文件按 mtime
    周期性清扫(默认 90 天, BOT_POLLER_MEDIA_RETENTION_DAYS 可调)——
    媒体字节=用户隐私数据, 不无限积累。daemon 线程 60s tick / 24h 执行。
    """

    def __init__(self, media_root=None, webhook_secret='', inbound_coalesce_window_ms=0):
        self._adapters = {}
        self._lock = threading.Lock()
        self._media_root = media_root
        self._inbound_coalesce_window_ms = 0
        self.configure_inbound_coalescing(inbound_coalesce_window_ms)
        self._webhook_secret = webhook_secret or ''
        if media_root:
            os.makedirs(media_root, exist_ok=True)
        # 媒体留存清扫 daemon(仅配置了 media_root 时启动)。
        self._sweep_thread = None
        if media_root:
            self._sweep_thread = threading.Thread(
                target=self._sweep_loop, daemon=True, name='media-sweep')
            self._sweep_thread.start()

    def _sweep_loop(self):
        """周期清扫 media_root 下超过保留期的媒体文件(24h 节流)。"""
        next_sweep = time.monotonic() + MEDIA_SWEEP_INTERVAL_SECONDS
        while True:
            time.sleep(MEDIA_SWEEP_TICK_SECONDS)
            if time.monotonic() < next_sweep:
                continue
            next_sweep = time.monotonic() + MEDIA_SWEEP_INTERVAL_SECONDS
            try:
                removed = self._sweep_media(MEDIA_RETENTION_DAYS)
                if removed:
                    print(f'[Poller] media sweep removed {removed} expired file(s) '
                          f'(>{MEDIA_RETENTION_DAYS}d)', flush=True)
            except Exception as exc:
                print(f'[Poller] media sweep err: {exc}', flush=True)

    def _sweep_media(self, retention_days):
        """删除 media_root/<bot_uuid>/ 下 mtime 超过保留期的文件; 返回删除数。

        只删 media_root 直接子目录(bot_uuid)内的普通文件, 不递归进入
        任意深层目录(防误删)。空 bot 目录顺带回收。幂等。"""
        if not self._media_root or not os.path.isdir(self._media_root):
            return 0
        cutoff = time.time() - retention_days * 86400
        removed = 0
        for bot_dir_name in os.listdir(self._media_root):
            bot_dir = os.path.join(self._media_root, bot_dir_name)
            if not os.path.isdir(bot_dir):
                continue
            try:
                for name in os.listdir(bot_dir):
                    path = os.path.join(bot_dir, name)
                    try:
                        if os.path.isfile(path) and os.path.getmtime(path) < cutoff:
                            os.remove(path)
                            removed += 1
                    except OSError:
                        pass
                if not os.listdir(bot_dir):
                    os.rmdir(bot_dir)  # 空目录回收
            except OSError:
                continue
        return removed

    def configure_inbound_coalescing(self, window_ms):
        window_ms = int(window_ms)
        if window_ms < 0 or window_ms > MAX_INBOUND_COALESCE_WINDOW_MS:
            raise ValueError(
                f'window_ms must be between 0 and {MAX_INBOUND_COALESCE_WINDOW_MS}'
            )
        with self._lock:
            self._inbound_coalesce_window_ms = window_ms
        return window_ms

    def _coalesce_window_ms(self):
        with self._lock:
            return self._inbound_coalesce_window_ms

    def start(self, bot_uuid, channel_type, config_json, *, base_url='', updates_buf='', webhook_url=''):
        """创建/重启一个渠道连接。幂等: 已存在的 bot_uuid 不重复启动。

        config_json 是解密后的渠道配置 JSON(wechat={token}, 新渠道=
        {app_id, app_secret})。
        """
        if not bot_uuid or not channel_type:
            raise ValueError('bot_uuid and channel_type are required')
        if channel_type not in VALID_CHANNEL_TYPES:
            raise ValueError(f'unsupported channel_type: {channel_type}')
        if isinstance(config_json, (str, bytes)):
            config = json.loads(config_json)
        else:
            config = config_json or {}
        with self._lock:
            if bot_uuid in self._adapters:
                return  # idempotent: frontend polls confirmed status repeatedly
        # 构造在锁外: WeChatAdapter 构造会经 coalesce_window_provider 回调
        # _coalesce_window_ms()(非重入锁, 持锁构造必然死锁)。
        adapter = self._build_adapter(bot_uuid, channel_type, config,
                                      base_url=base_url, updates_buf=updates_buf,
                                      webhook_url=webhook_url)
        adapter.start()
        with self._lock:
            if bot_uuid in self._adapters:
                # 并发重复 start 输给了其他线程: 停掉本次重复连接。
                adapter.stop()
                return
            self._adapters[bot_uuid] = adapter

    def _build_adapter(self, bot_uuid, channel_type, config, *, base_url, updates_buf, webhook_url):
        # 审查 I-3: 全部渠道共用平台级入站合并窗口(微信此前独享)。
        common = dict(media_root=self._media_root, webhook_secret=self._webhook_secret,
                      coalesce_window_provider=self._coalesce_window_ms)
        if channel_type == CHANNEL_WECHAT:
            return WeChatAdapter(
                bot_uuid, config, webhook_url,
                base_url=base_url, updates_buf=updates_buf,
                **common,
            )
        if channel_type == CHANNEL_FEISHU:
            return FeishuAdapter(bot_uuid, config, webhook_url, **common)
        if channel_type == CHANNEL_DINGTALK:
            return DingTalkAdapter(bot_uuid, config, webhook_url, **common)
        if channel_type == CHANNEL_QQ:
            return QQAdapter(bot_uuid, config, webhook_url, **common)
        if channel_type == CHANNEL_WECOM:
            return WeComAdapter(bot_uuid, config, webhook_url, **common)
        raise ValueError(f'unsupported channel_type: {channel_type}')

    def stop(self, bot_uuid):
        with self._lock:
            adapter = self._adapters.pop(bot_uuid, None)
        if not adapter:
            return ''
        return adapter.stop()

    def send(self, bot_uuid, channel_account_id, text, context_token='', msg_type=MSG_TYPE_TEXT,
             file_path='', file_name='', client_id=''):
        """按 bot_uuid 路由到对应渠道 adapter(回复目标=channel_account_id)。"""
        with self._lock:
            adapter = self._adapters.get(bot_uuid)
        if not adapter:
            raise KeyError(f'bot {bot_uuid} not running')
        if msg_type == MSG_TYPE_TEXT or not msg_type:
            adapter.send_text(channel_account_id, text, client_id=client_id)
            return
        if not file_path:
            raise ValueError(f'file_path is required for msg_type={msg_type}')
        # 统一出站媒体接口(IM_MEDIA_ARCHITECTURE §5.1 A1): msg_type 即
        # media_type(image|file|video)。未实现渠道抛 NotImplementedError →
        # /send 500 → Go delivery 重试/死信(fail-closed, 决策 A3)。
        adapter.send_media(channel_account_id, file_path, msg_type,
                           file_name=file_name, client_id=client_id)

    def send_stream(self, bot_uuid, stream_id, action, target='', text=''):
        """IM 流式出站(IM_STREAMING_DELIVERY §4.2): /send 扩展 stream_action。

        action ∈ open|append|commit|abort。open 返回渠道侧 stream_id
        (飞书=占位消息 message_id 关联的流句柄), 其余动作按 stream_id
        路由。非流渠道抛 NotImplementedError(Go 侧 BeginReply 失败回退
        终态 delivery)。
        """
        with self._lock:
            adapter = self._adapters.get(bot_uuid)
        if not adapter:
            raise KeyError(f'bot {bot_uuid} not running')
        if action == 'open':
            return adapter.send_stream_open(target, text)
        if action == 'append':
            if not stream_id:
                raise ValueError('stream_id is required for stream_action=append')
            return adapter.send_stream_append(stream_id, text)
        if action == 'commit':
            if not stream_id:
                raise ValueError('stream_id is required for stream_action=commit')
            return adapter.send_stream_commit(stream_id, text)
        if action == 'abort':
            if not stream_id:
                raise ValueError('stream_id is required for stream_action=abort')
            return adapter.send_stream_abort(stream_id)
        raise ValueError(f'unsupported stream_action: {action}')

    def health(self):
        with self._lock:
            return {'healthy': True, 'active_bots': list(self._adapters.keys())}


class PollerHandler(BaseHTTPRequestHandler):
    """HTTP API: /start /stop /send /health /config."""

    manager = None  # set by serve()
    api_secret = ''  # set by serve(); HMAC-SHA256 shared secret for inbound API auth

    def log_message(self, fmt, *args):
        pass  # silence default access log

    def _reply(self, code, obj):
        data = json.dumps(obj).encode('utf-8')
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _read_json(self):
        length = int(self.headers.get('Content-Length', 0))
        if length > MAX_BODY_BYTES:
            raise ValueError(f'Content-Length {length} exceeds limit {MAX_BODY_BYTES}')
        if length == 0:
            return {}
        body = self.rfile.read(length)
        if len(body) != length:
            raise ValueError('request body truncated')
        return json.loads(body)

    def _verify_request_signature(self, body_bytes):
        """Verify X-API-Signature header against HMAC-SHA256(body_bytes, api_secret).

        Returns True if valid or api_secret is empty (loopback dev mode —
        serve() refuses to start with empty secret on non-loopback binds,
        so an empty secret here implies loopback dev only).
        Returns False if signature is missing or mismatched.
        """
        if not self.api_secret:
            return True  # loopback dev only: serve() enforces non-loopback fail-closed

        received_sig = self.headers.get('X-API-Signature', '')
        if not received_sig:
            return False

        expected_sig = hmac.new(
            self.api_secret.encode('utf-8'),
            body_bytes,
            hashlib.sha256,
        ).hexdigest()
        return hmac.compare_digest(received_sig, expected_sig)

    def do_GET(self):
        if self.path == '/health':
            self._reply(200, self.manager.health())
        else:
            self._reply(404, {'error': 'not found'})

    def do_POST(self):
        try:
            length = int(self.headers.get('Content-Length', 0))
            if length > MAX_BODY_BYTES:
                self._reply(413, {'error': 'request body too large'})
                return
            body_bytes = self.rfile.read(length) if length > 0 else b'{}'
            if len(body_bytes) != length:
                self._reply(400, {'error': 'request body truncated'})
                return

            # Verify signature before processing (except /health is GET-only)
            if not self._verify_request_signature(body_bytes):
                self._reply(401, {'error': 'invalid or missing X-API-Signature'})
                return

            body = json.loads(body_bytes) if body_bytes != b'{}' else {}

            if self.path == '/config':
                window_ms = self.manager.configure_inbound_coalescing(
                    body.get('inbound_coalesce_window_ms', 0)
                )
                self._reply(200, {'inbound_coalesce_window_ms': window_ms})
            elif self.path == '/start':
                self.manager.start(
                    body['bot_uuid'], body['channel_type'],
                    body.get('config_json', '{}'),
                    base_url=body.get('base_url', ''),
                    updates_buf=body.get('updates_buf', ''),
                    webhook_url=body['webhook_url'])
                self._reply(200, {'started': True})
            elif self.path == '/stop':
                buf = self.manager.stop(body['bot_uuid'])
                self._reply(200, {'stopped': True, 'updates_buf': buf})
            elif self.path == '/send':
                msg_type = body.get('msg_type') or MSG_TYPE_TEXT
                if msg_type not in _VALID_MSG_TYPES:
                    self._reply(400, {'error': f'invalid msg_type: {msg_type}'})
                    return
                # IM 流式输出(IM_STREAMING_DELIVERY §4.2): /send 扩展
                # stream_id + stream_action(open|append|commit|abort),
                # 复用既有通道不新增端点。open 响应携带渠道侧 stream_id。
                stream_action = body.get('stream_action') or ''
                if stream_action:
                    stream_id = self.manager.send_stream(
                        body['bot_uuid'], body.get('stream_id', ''), stream_action,
                        target=body.get('channel_account_id', ''),
                        text=body.get('text', ''),
                    )
                    reply = {'sent': True}
                    if stream_action == 'open':
                        reply['stream_id'] = stream_id
                    self._reply(200, reply)
                    return
                self.manager.send(body['bot_uuid'], body['channel_account_id'],
                                  body.get('text', ''), body.get('context_token', ''),
                                  msg_type=msg_type, file_path=body.get('file_path', ''),
                                  file_name=body.get('file_name', ''),
                                  client_id=body.get('client_id', ''))
                self._reply(200, {'sent': True})
            else:
                self._reply(404, {'error': 'not found'})
        except KeyError as exc:
            self._reply(400, {'error': f'missing field: {exc}'})
        except ValueError as exc:
            # 请求体解析/格式错误(含超限), 不区分内部细节。
            self._reply(400, {'error': str(exc) if 'exceeds limit' in str(exc) or 'truncated' in str(exc) else 'invalid request body'})
        except Exception as exc:
            # 审查: 不把内部异常细节(含路径)透出给调用方。
            print(f'[poller] internal error handling {self.path}: {exc!r}', flush=True)
            self._reply(500, {'error': 'internal error'})


def serve(listen, media_root=None, webhook_secret='', api_secret=''):
    PollerHandler.manager = BotManager(media_root=media_root, webhook_secret=webhook_secret)
    PollerHandler.api_secret = api_secret or ''
    host, port = _parse_listen_addr(listen)
    loopback = host in ('127.0.0.1', '::1', 'localhost')
    if not api_secret and not loopback:
        # 审查: 非回环绑定 + 空 secret 即完全裸奔(/start 可注入 bot_token、
        # /send 可代发消息)——fail-closed 拒绝启动, 与 Go 侧 im_webhook 一致。
        raise SystemExit(
            f'bot_poller refuses to listen on {host}:{port} without --api-secret '
            '(non-loopback bind requires API auth; pass a shared secret)'
        )
    if not api_secret:
        print('WARNING: bot_poller running WITHOUT --api-secret (loopback dev only; ' + \
              'any local process can control bots)', flush=True)
    server = ThreadingHTTPServer((host, port), PollerHandler)
    server.timeout = HTTP_READ_TIMEOUT  # 空闲连接读超时, 防慢速/死连接占线程
    server.request_queue_size = HTTP_REQUEST_QUEUE_SIZE
    auth_status = 'on' if api_secret else 'off (INSECURE - loopback dev only)'
    print(f'bot_poller listening on {host}:{port} (media_root={media_root or "disabled"}, api_auth={auth_status}, webhook_auth={"on" if webhook_secret else "off"})', flush=True)

    # serve_forever() blocks the main thread; SIGINT/SIGTERM raise KeyboardInterrupt
    # on Windows (SIGTERM) or interrupt the call (SIGINT), letting us shut down cleanly.
    try:
        server.serve_forever(poll_interval=0.5)
    except KeyboardInterrupt:
        pass
    finally:
        server.shutdown()
        server.server_close()


def main(argv=None):
    parser = argparse.ArgumentParser(description='GenericAgent Bot Poller')
    parser.add_argument('--listen', default=os.environ.get('BOT_POLLER_LISTEN', '127.0.0.1:8090'))
    parser.add_argument('--media-dir', default=os.environ.get('BOT_POLLER_MEDIA_DIR', ''),
                        help='Root directory for inbound media files. Empty disables media download.')
    parser.add_argument('--webhook-secret', default=os.environ.get('PLATFORM_WEBHOOK_SECRET', ''),
                        help='HMAC-SHA256 secret shared with the Go platform to sign /v1/im/webhook requests (or PLATFORM_WEBHOOK_SECRET). Empty = unauthenticated (dev/test only).')
    parser.add_argument('--api-secret', default=os.environ.get('BOT_POLLER_API_SECRET', ''),
                        help='HMAC-SHA256 secret for authenticating inbound /start /stop /send requests (or BOT_POLLER_API_SECRET). Empty = unauthenticated (INSECURE - dev/test only).')
    args = parser.parse_args(argv)
    serve(args.listen,
          media_root=args.media_dir or None,
          webhook_secret=args.webhook_secret or '',
          api_secret=args.api_secret or '')


if __name__ == '__main__':
    main()
