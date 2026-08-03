"""Bot Poller: multi-tenant WeChat iLink long-poll service.

Reuses frontends/wxbot_client.WxBotClient (the verified GA Core protocol
implementation) so the platform never re-implements iLink getupdates/sendmessage.

Architecture:
    Go platform (control plane)  ──HTTP──▶  Bot Poller (this process)
         │                                    │
         │  /v1/im/webhook ◀──HTTP POST────  │  (inbound messages + media_paths)
         │                                    │
         └── /send ──▶ Poller.send_text/send_image/send_video/send_file ──▶ iLink

Each active bot runs in its own thread via WxBotClient.get_updates.
Token is injected at start time (decrypted by Go); nothing is written to disk
except inbound media files, which land under --media-dir/{bot_uuid}/.

iLink officially supports 4 media types (image/voice/file/video) for both
send and receive (see docs/superpowers/specs/2026-07-25-ilink-official-binding-flow.md).
GA Core's WxBotClient implements send_image/send_video/send_file and
wxbot_media.download_media covers all 4 inbound types.
"""

import argparse
import collections
import hashlib
import hmac
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import requests

# Make frontends/ importable for wxbot_client and wxbot_media.
# _POLLER_DIR = tenant_platform/bot_poller
# _LEGACY_ROOT = GenericAgent (two levels up)
# _FRONTENDS_DIR = GenericAgent/frontends (where wxbot_client.py and wxbot_media.py live)
_POLLER_DIR = os.path.dirname(os.path.abspath(__file__))
_LEGACY_ROOT = os.path.dirname(os.path.dirname(_POLLER_DIR))
_FRONTENDS_DIR = os.path.join(_LEGACY_ROOT, 'frontends')
for _p in (_FRONTENDS_DIR, _LEGACY_ROOT):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from wxbot_client import WxBotClient, AuthExpired  # noqa: E402
from wxbot_media import download_media  # noqa: E402

POLL_TIMEOUT = 30
WEBHOOK_TIMEOUT = 10
# Webhook delivery retry: exponential backoff base/cap. Retrying blocks the
# bot's dispatch loop on purpose — that is the backpressure that stops the
# cursor from advancing past an undelivered message (same model as a Kafka
# consumer that refuses to commit an offset it failed to process).
WEBHOOK_RETRY_BASE_SECONDS = 2.0
WEBHOOK_RETRY_CAP_SECONDS = 60.0
MAX_INBOUND_COALESCE_WINDOW_MS = 5000
MAX_COALESCED_MESSAGES = 8

# Map file extension to MIME content_type. Used to populate media_assets
# metadata when the Poller forwards inbound media to the platform webhook.
# iLink does not surface a content_type field in item_list, so we infer from
# the file_name returned by wxbot_media.download_media.
_EXT_CONTENT_TYPES = {
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


def _guess_content_type(file_name):
    """Infer MIME type from file extension. Defaults to octet-stream."""
    ext = os.path.splitext(file_name)[1].lower()
    return _EXT_CONTENT_TYPES.get(ext, 'application/octet-stream')


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
    if previous.get('ilink_user_id') != current.get('ilink_user_id'):
        return False
    previous_at = int(previous.get('_received_at_ms') or 0)
    current_at = int(current.get('_received_at_ms') or 0)
    return current_at >= previous_at and current_at - previous_at <= window_ms


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


def coalesce_webhook_bodies(bodies, window_ms):
    """Merge adjacent ordinary messages supplied as one sequence.

    Same-user messages inside the configured window may carry different
    per-message context tokens. They still belong to the same user action;
    the finalized webhook keeps the newest token for subsequent replies.
    Commands always remain standalone. The hard part cap prevents one burst
    from collapsing into an unbounded prompt.
    """
    if window_ms <= 0:
        return [_finalize_coalesced_group([body]) for body in bodies]
    groups = []
    current = []
    for body in bodies:
        if current and (
            len(current) >= MAX_COALESCED_MESSAGES
            or not _can_coalesce(current[-1], body, window_ms)
        ):
            groups.append(current)
            current = []
        current.append(body)
        if _is_command_body(body):
            groups.append(current)
            current = []
    if current:
        groups.append(current)
    return [_finalize_coalesced_group(group) for group in groups]


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
            ready.extend(coalesce_webhook_bodies(bodies, 0))
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


class BotEntry:
    """Per-bot runtime state held by the poller."""

    def __init__(self, client, webhook_url, bot_uuid, coalesce_window_ms=0):
        self.client = client
        self.webhook_url = webhook_url
        self.bot_uuid = bot_uuid
        self.stop_event = threading.Event()
        self.thread = None
        self.auth_expired = False
        self.committed_updates_buf = getattr(client, 'updates_buf', '') or ''
        self.webhook_idle = threading.Event()
        self.webhook_idle.set()
        self.coalescer = InboundCoalescingBuffer(coalesce_window_ms)


class BotManager:
    """Manages long-poll threads for multiple bots."""

    def __init__(self, media_root=None, webhook_secret='', inbound_coalesce_window_ms=0):
        self._bots = {}
        self._lock = threading.Lock()
        self._media_root = media_root
        self._inbound_coalesce_window_ms = 0
        self.configure_inbound_coalescing(inbound_coalesce_window_ms)
        # HMAC-SHA256 secret shared with the Go platform. When set, every
        # webhook POST carries X-Webhook-Signature = hex(HMAC-SHA256(body)).
        # Empty = unauthenticated (dev/test only; Go side logs a warning).
        self._webhook_secret = webhook_secret or ''
        if media_root:
            os.makedirs(media_root, exist_ok=True)

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

    def start(self, bot_uuid, bot_token, ilink_bot_id, base_url, updates_buf, webhook_url):
        with self._lock:
            if bot_uuid in self._bots:
                return  # idempotent: frontend polls confirmed status repeatedly
            client = WxBotClient(token=bot_token, persist=False, base_url=base_url or None)
            client.bot_id = ilink_bot_id
            client.updates_buf = updates_buf or ''
            entry = BotEntry(
                client=client,
                webhook_url=webhook_url,
                bot_uuid=bot_uuid,
                coalesce_window_ms=self._inbound_coalesce_window_ms,
            )
            entry.media_dir = self._bot_media_dir(bot_uuid)
            entry.thread = threading.Thread(target=self._run, args=(entry,), daemon=True)
            entry.thread.start()
            self._bots[bot_uuid] = entry

    def _bot_media_dir(self, bot_uuid):
        if not self._media_root:
            return None
        d = os.path.join(self._media_root, bot_uuid)
        os.makedirs(d, exist_ok=True)
        return d

    def _run(self, entry):
        """Long-poll loop for one bot. Exits on stop_event or AuthExpired."""
        # Bounded FIFO dedup window, local to this bot's thread (no sharing).
        # The old implementation trimmed with `seen = set(list(seen)[-2000:])`
        # inside _dispatch, which only rebound the local parameter — the
        # caller's set was never trimmed, so memory grew without bound.
        # set gives O(1) lookup; deque(maxlen) evicts oldest ids in FIFO order.
        # The platform's (bot_id, message_id) idempotency key remains the
        # final defense; this window only avoids redundant webhook POSTs.
        seen = _DedupWindow(maxlen=2000)
        while not entry.stop_event.is_set():
            try:
                now_ms = int(time.time() * 1000)
                entry.coalescer.set_window(self._coalesce_window_ms())
                for body in entry.coalescer.flush_due(now_ms):
                    self._post_webhook_body(entry, body)
                request_timeout = entry.coalescer.timeout_seconds(now_ms)
                messages = entry.client.get_updates(
                    POLL_TIMEOUT, request_timeout=request_timeout
                )
                self._dispatch_batch(entry, messages, seen)
            except AuthExpired:
                entry.auth_expired = True
                self._notify_expired(entry)
                break
            except Exception as exc:  # network jitter: back off and retry
                print(f'[Poller] bot {entry.bot_uuid} err: {exc}', flush=True)
                entry.stop_event.wait(5)

    def _dispatch_batch(self, entry, messages, seen):
        received_at_ms = int(time.time() * 1000)
        bodies = []
        for msg in messages:
            body = self._prepare_webhook_body(entry, msg, seen, received_at_ms)
            if body is not None:
                bodies.append(body)
        entry.coalescer.set_window(self._coalesce_window_ms())
        ready = entry.coalescer.push(bodies, received_at_ms)
        ready.extend(entry.coalescer.flush_due(received_at_ms))
        for body in ready:
            self._post_webhook_body(entry, body)

    def _dispatch(self, entry, msg, seen):
        """Compatibility wrapper for a single inbound message."""
        body = self._prepare_webhook_body(entry, msg, seen, int(time.time() * 1000))
        if body is not None:
            self._post_webhook_body(entry, _finalize_coalesced_group([body]))

    def _prepare_webhook_body(self, entry, msg, seen, fallback_time_ms):
        """Download media and build one platform webhook body."""
        mid = str(msg.get('message_id', 0))
        if not entry.client.is_user_msg(msg) or not seen.add(mid):
            return None
        text = entry.client.extract_text(msg)
        uid = msg.get('from_user_id', '')
        ctx = msg.get('context_token', '')
        media_paths = []
        media_items = []
        if entry.media_dir:
            try:
                media_paths = download_media(msg.get('item_list', []), dest_dir=entry.media_dir)
                media_items = self._collect_media_items(media_paths)
            except Exception as exc:
                print(f'[Poller] media dl err ({entry.bot_uuid}): {exc}', flush=True)
        return {
            'bot_uuid': entry.bot_uuid,
            'ilink_user_id': uid,
            'message_id': mid,
            'text': text,
            'context_token': ctx,
            'updates_buf': entry.client.updates_buf,
            'media_paths': media_paths or [],
            'media_items': media_items or [],
            '_received_at_ms': _message_time_ms(msg, fallback_time_ms),
        }

    def _collect_media_items(self, paths):
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
        for path in paths:
            try:
                size = os.path.getsize(path)
            except OSError:
                size = 0
            file_name = os.path.basename(path)
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

    def _post_webhook(self, entry, uid, mid, text, ctx, media_paths, media_items):
        body = {
            'bot_uuid': entry.bot_uuid,
            'ilink_user_id': uid,
            'message_id': mid,
            'text': text,
            'context_token': ctx,
            'updates_buf': entry.client.updates_buf,
            'media_paths': media_paths or [],
            'media_items': media_items or [],
        }
        self._post_webhook_body(entry, body)

    def _notify_expired(self, entry):
        body = {'bot_uuid': entry.bot_uuid, 'auth_expired': True}
        # Bounded attempts: the bot loop is exiting either way; the platform
        # also detects expiry via send failures, so losing this signal is
        # recoverable and must not wedge the thread forever.
        self._post_webhook_body(entry, body, max_attempts=5)

    def _post_webhook_body(self, entry, body, max_attempts=None):
        entry.webhook_idle.clear()
        try:
            return self._post_webhook_body_inner(entry, body, max_attempts=max_attempts)
        finally:
            entry.webhook_idle.set()

    def _post_webhook_body_inner(self, entry, body, max_attempts=None):
        """POST webhook with deterministic JSON + HMAC-SHA256 signature.

        We serialize once and send as raw bytes so the signature matches the
        exact bytes on the wire (requests' json= would re-serialize and could
        diverge on key ordering/whitespace across versions).

        Delivery contract (matches im_webhook.go, which returns 5xx expecting
        a retry, e.g. CURSOR_PERSIST_FAILED):
          - 2xx  -> delivered, return True.
          - 4xx  -> permanent rejection (bad signature / validation); log
                    loudly and drop, return False. Retrying cannot heal it.
          - 5xx / network error -> retry with capped exponential backoff,
                    blocking this bot's loop (backpressure keeps ordering and
                    stops the cursor from advancing past an undelivered
                    message). Platform (bot_id, message_id) dedup absorbs any
                    resulting at-least-once redelivery.

        max_attempts=None retries until stop_event is set.
        Returns True when delivered, False when dropped or interrupted.
        """
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
        while not entry.stop_event.is_set():
            attempt += 1
            try:
                resp = requests.post(entry.webhook_url, data=body_bytes,
                                     headers=headers, timeout=WEBHOOK_TIMEOUT)
                if 200 <= resp.status_code < 300:
                    committed_cursor = body.get('updates_buf', '')
                    if committed_cursor:
                        entry.committed_updates_buf = committed_cursor
                    return True
                if 400 <= resp.status_code < 500:
                    print(f'[Poller] webhook PERMANENTLY rejected ({entry.bot_uuid}) '
                          f'status={resp.status_code} body={resp.text[:200]} — message dropped',
                          flush=True)
                    return False
                err_desc = f'status={resp.status_code} body={resp.text[:200]}'
            except Exception as exc:
                err_desc = f'error={exc}'

            if max_attempts is not None and attempt >= max_attempts:
                print(f'[Poller] webhook delivery gave up after {attempt} attempts '
                      f'({entry.bot_uuid}): {err_desc}', flush=True)
                return False
            backoff = min(WEBHOOK_RETRY_BASE_SECONDS * (2 ** (attempt - 1)),
                          WEBHOOK_RETRY_CAP_SECONDS)
            print(f'[Poller] webhook post failed ({entry.bot_uuid}) attempt={attempt} '
                  f'{err_desc}; retrying in {backoff:.0f}s', flush=True)
            entry.stop_event.wait(backoff)
        return False

    def stop(self, bot_uuid):
        with self._lock:
            entry = self._bots.pop(bot_uuid, None)
        if not entry:
            return ''
        entry.stop_event.set()
        entry.webhook_idle.wait(timeout=WEBHOOK_TIMEOUT + 1)
        entry.thread.join(timeout=5)
        return entry.committed_updates_buf

    def send(self, bot_uuid, ilink_user_id, text, context_token='', msg_type=MSG_TYPE_TEXT, file_path='', file_name=''):
        """Dispatch to send_text/send_image/send_video/send_file based on msg_type.

        iLink officially supports image/video/file media sends (see spec).
        Voice send is not implemented in GA Core's WxBotClient, so 'voice'
        is not a valid msg_type here (inbound voice still downloads fine).
        file_name 是用户可见显示名(审查 R5-I10): 与 file_path 分离, 快照临时
        文件名含 marker hash 前缀, 不得作为显示名暴露给用户。
        """
        with self._lock:
            entry = self._bots.get(bot_uuid)
        if not entry:
            raise KeyError(f'bot {bot_uuid} not running')
        if msg_type == MSG_TYPE_TEXT or not msg_type:
            entry.client.send_text(ilink_user_id, text, context_token=context_token)
            return
        if not file_path:
            raise ValueError(f'file_path is required for msg_type={msg_type}')
        if msg_type == MSG_TYPE_IMAGE:
            entry.client.send_image(ilink_user_id, file_path, context_token=context_token)
        elif msg_type == MSG_TYPE_VIDEO:
            entry.client.send_video(ilink_user_id, file_path, context_token=context_token)
        elif msg_type == MSG_TYPE_FILE:
            entry.client.send_file(ilink_user_id, file_path, context_token=context_token, file_name=file_name)
        else:
            raise ValueError(f'unsupported msg_type: {msg_type}')

    def health(self):
        with self._lock:
            return {'healthy': True, 'active_bots': list(self._bots.keys())}


class PollerHandler(BaseHTTPRequestHandler):
    """HTTP API: /start /stop /send /health."""

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
        if length == 0:
            return {}
        return json.loads(self.rfile.read(length))

    def _verify_request_signature(self, body_bytes):
        """Verify X-API-Signature header against HMAC-SHA256(body_bytes, api_secret).

        Returns True if valid or api_secret is empty (dev/test mode).
        Returns False if signature is missing or mismatched.
        """
        if not self.api_secret:
            return True  # no auth configured, allow (dev/test only)

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
            body_bytes = self.rfile.read(length) if length > 0 else b'{}'

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
                    body['bot_uuid'], body['bot_token'], body.get('ilink_bot_id', ''),
                    body.get('base_url', ''), body.get('updates_buf', ''),
                    body['webhook_url'])
                self._reply(200, {'started': True})
            elif self.path == '/stop':
                buf = self.manager.stop(body['bot_uuid'])
                self._reply(200, {'stopped': True, 'updates_buf': buf})
            elif self.path == '/send':
                msg_type = body.get('msg_type') or MSG_TYPE_TEXT
                if msg_type not in _VALID_MSG_TYPES:
                    self._reply(400, {'error': f'invalid msg_type: {msg_type}'})
                    return
                self.manager.send(body['bot_uuid'], body['ilink_user_id'],
                                  body.get('text', ''), body.get('context_token', ''),
                                  msg_type=msg_type, file_path=body.get('file_path', ''),
                                  file_name=body.get('file_name', ''))
                self._reply(200, {'sent': True})
            else:
                self._reply(404, {'error': 'not found'})
        except KeyError as exc:
            self._reply(400, {'error': f'missing field: {exc}'})
        except Exception as exc:
            self._reply(500, {'error': str(exc)})


def serve(listen, grace_seconds=10.0, media_root=None, webhook_secret='', api_secret=''):
    PollerHandler.manager = BotManager(media_root=media_root, webhook_secret=webhook_secret)
    PollerHandler.api_secret = api_secret or ''
    host, port = _parse_listen_addr(listen)
    server = ThreadingHTTPServer((host, port), PollerHandler)
    auth_status = 'on' if api_secret else 'off (INSECURE - dev/test only)'
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
    parser.add_argument('--grace-seconds', type=float, default=10.0)
    parser.add_argument('--media-dir', default=os.environ.get('BOT_POLLER_MEDIA_DIR', ''),
                        help='Root directory for inbound media files. Empty disables media download.')
    parser.add_argument('--webhook-secret', default=os.environ.get('PLATFORM_WEBHOOK_SECRET', ''),
                        help='HMAC-SHA256 secret shared with the Go platform to sign /v1/im/webhook requests (or PLATFORM_WEBHOOK_SECRET). Empty = unauthenticated (dev/test only).')
    parser.add_argument('--api-secret', default=os.environ.get('BOT_POLLER_API_SECRET', ''),
                        help='HMAC-SHA256 secret for authenticating inbound /start /stop /send requests (or BOT_POLLER_API_SECRET). Empty = unauthenticated (INSECURE - dev/test only).')
    args = parser.parse_args(argv)
    serve(args.listen, args.grace_seconds,
          media_root=args.media_dir or None,
          webhook_secret=args.webhook_secret or '',
          api_secret=args.api_secret or '')


if __name__ == '__main__':
    main()
