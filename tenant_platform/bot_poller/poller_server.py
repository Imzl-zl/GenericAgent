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
import hashlib
import hmac
import json
import os
import sys
import threading
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


class BotEntry:
    """Per-bot runtime state held by the poller."""

    def __init__(self, client, webhook_url, bot_uuid):
        self.client = client
        self.webhook_url = webhook_url
        self.bot_uuid = bot_uuid
        self.stop_event = threading.Event()
        self.thread = None
        self.auth_expired = False


class BotManager:
    """Manages long-poll threads for multiple bots."""

    def __init__(self, media_root=None, webhook_secret=''):
        self._bots = {}
        self._lock = threading.Lock()
        self._media_root = media_root
        # HMAC-SHA256 secret shared with the Go platform. When set, every
        # webhook POST carries X-Webhook-Signature = hex(HMAC-SHA256(body)).
        # Empty = unauthenticated (dev/test only; Go side logs a warning).
        self._webhook_secret = webhook_secret or ''
        if media_root:
            os.makedirs(media_root, exist_ok=True)

    def start(self, bot_uuid, bot_token, ilink_bot_id, base_url, updates_buf, webhook_url):
        with self._lock:
            if bot_uuid in self._bots:
                return  # idempotent: frontend polls confirmed status repeatedly
            client = WxBotClient(token=bot_token, persist=False, base_url=base_url or None)
            client.bot_id = ilink_bot_id
            client.updates_buf = updates_buf or ''
            entry = BotEntry(client=client, webhook_url=webhook_url, bot_uuid=bot_uuid)
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
        seen = set()
        while not entry.stop_event.is_set():
            try:
                for msg in entry.client.get_updates(POLL_TIMEOUT):
                    self._dispatch(entry, msg, seen)
            except AuthExpired:
                entry.auth_expired = True
                self._notify_expired(entry)
                break
            except Exception as exc:  # network jitter: back off and retry
                print(f'[Poller] bot {entry.bot_uuid} err: {exc}', flush=True)
                entry.stop_event.wait(5)

    def _dispatch(self, entry, msg, seen):
        """Forward one inbound message to the platform webhook.

        Downloads any media items via wxbot_media.download_media and includes
        the resulting local file paths in the webhook body as `media_paths`
        (for the worker to read) plus `media_items` (metadata for the
        platform to persist in media_assets). Text is still extracted so
        text-only routing (commands, /stop, etc.) keeps working when a
        message contains both text and media items.
        """
        mid = str(msg.get('message_id', 0))
        if not entry.client.is_user_msg(msg) or mid in seen:
            return
        seen.add(mid)
        if len(seen) > 5000:
            seen = set(list(seen)[-2000:])
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
        self._post_webhook(entry, uid, mid, text, ctx, media_paths, media_items)

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
        self._post_webhook_body(entry, body)

    def _post_webhook_body(self, entry, body):
        """POST webhook with deterministic JSON + HMAC-SHA256 signature.

        We serialize once and send as raw bytes so the signature matches the
        exact bytes on the wire (requests' json= would re-serialize and could
        diverge on key ordering/whitespace across versions).
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
        try:
            requests.post(entry.webhook_url, data=body_bytes, headers=headers, timeout=WEBHOOK_TIMEOUT)
        except Exception as exc:
            print(f'[Poller] webhook post err ({entry.bot_uuid}): {exc}', flush=True)

    def stop(self, bot_uuid):
        with self._lock:
            entry = self._bots.pop(bot_uuid, None)
        if not entry:
            return ''
        entry.stop_event.set()
        entry.thread.join(timeout=5)
        return entry.client.updates_buf

    def send(self, bot_uuid, ilink_user_id, text, context_token='', msg_type=MSG_TYPE_TEXT, file_path=''):
        """Dispatch to send_text/send_image/send_video/send_file based on msg_type.

        iLink officially supports image/video/file media sends (see spec).
        Voice send is not implemented in GA Core's WxBotClient, so 'voice'
        is not a valid msg_type here (inbound voice still downloads fine).
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
            entry.client.send_file(ilink_user_id, file_path, context_token=context_token)
        else:
            raise ValueError(f'unsupported msg_type: {msg_type}')

    def health(self):
        with self._lock:
            return {'healthy': True, 'active_bots': list(self._bots.keys())}


class PollerHandler(BaseHTTPRequestHandler):
    """HTTP API: /start /stop /send /health."""

    manager = None  # set by serve()

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

    def do_GET(self):
        if self.path == '/health':
            self._reply(200, self.manager.health())
        else:
            self._reply(404, {'error': 'not found'})

    def do_POST(self):
        try:
            body = self._read_json()
            if self.path == '/start':
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
                                  msg_type=msg_type, file_path=body.get('file_path', ''))
                self._reply(200, {'sent': True})
            else:
                self._reply(404, {'error': 'not found'})
        except KeyError as exc:
            self._reply(400, {'error': f'missing field: {exc}'})
        except Exception as exc:
            self._reply(500, {'error': str(exc)})


def serve(listen, grace_seconds=10.0, media_root=None, webhook_secret=''):
    PollerHandler.manager = BotManager(media_root=media_root, webhook_secret=webhook_secret)
    host, port = _parse_listen_addr(listen)
    server = ThreadingHTTPServer((host, port), PollerHandler)
    print(f'bot_poller listening on {host}:{port} (media_root={media_root or "disabled"}, auth={"on" if webhook_secret else "off"})', flush=True)

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
    args = parser.parse_args(argv)
    serve(args.listen, args.grace_seconds,
          media_root=args.media_dir or None,
          webhook_secret=args.webhook_secret or '')


if __name__ == '__main__':
    main()
