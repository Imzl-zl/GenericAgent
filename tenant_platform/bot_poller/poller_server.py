"""Bot Poller: multi-tenant WeChat iLink long-poll service.

Reuses frontends/wxbot_client.WxBotClient (the verified GA Core protocol
implementation) so the platform never re-implements iLink getupdates/sendmessage.

Architecture:
    Go platform (control plane)  ──HTTP──▶  Bot Poller (this process)
         │                                    │
         │  /v1/im/webhook ◀──HTTP POST────  │  (inbound messages)
         │                                    │
         └── /send ──▶ Poller.send_text ──▶  iLink

Each active bot runs in its own thread via WxBotClient.get_updates.
Token is injected at start time (decrypted by Go); nothing is written to disk.
"""

import argparse
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import requests

# Make frontends/ importable for wxbot_client.
_POLLER_DIR = os.path.dirname(os.path.abspath(__file__))
_LEGACY_ROOT = os.path.dirname(os.path.dirname(_POLLER_DIR))
if _LEGACY_ROOT not in sys.path:
    sys.path.insert(0, _LEGACY_ROOT)

from wxbot_client import WxBotClient, AuthExpired  # noqa: E402

POLL_TIMEOUT = 30
WEBHOOK_TIMEOUT = 10


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

    def __init__(self):
        self._bots = {}
        self._lock = threading.Lock()

    def start(self, bot_uuid, bot_token, ilink_bot_id, base_url, updates_buf, webhook_url):
        with self._lock:
            if bot_uuid in self._bots:
                return  # idempotent: frontend polls confirmed status repeatedly
            client = WxBotClient(token=bot_token, persist=False, base_url=base_url or None)
            client.bot_id = ilink_bot_id
            client.updates_buf = updates_buf or ''
            entry = BotEntry(client=client, webhook_url=webhook_url, bot_uuid=bot_uuid)
            entry.thread = threading.Thread(target=self._run, args=(entry,), daemon=True)
            entry.thread.start()
            self._bots[bot_uuid] = entry

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
        """Forward one inbound message to the platform webhook."""
        mid = str(msg.get('message_id', 0))
        if not entry.client.is_user_msg(msg) or mid in seen:
            return
        seen.add(mid)
        if len(seen) > 5000:
            seen = set(list(seen)[-2000:])
        text = entry.client.extract_text(msg)
        uid = msg.get('from_user_id', '')
        ctx = msg.get('context_token', '')
        self._post_webhook(entry, uid, mid, text, ctx)

    def _post_webhook(self, entry, uid, mid, text, ctx):
        body = {
            'bot_uuid': entry.bot_uuid,
            'ilink_user_id': uid,
            'message_id': mid,
            'text': text,
            'context_token': ctx,
            'updates_buf': entry.client.updates_buf,
        }
        try:
            requests.post(entry.webhook_url, json=body, timeout=WEBHOOK_TIMEOUT)
        except Exception as exc:
            print(f'[Poller] webhook post err ({entry.bot_uuid}): {exc}', flush=True)

    def _notify_expired(self, entry):
        body = {'bot_uuid': entry.bot_uuid, 'auth_expired': True}
        try:
            requests.post(entry.webhook_url, json=body, timeout=WEBHOOK_TIMEOUT)
        except Exception as exc:
            print(f'[Poller] expired-notify err ({entry.bot_uuid}): {exc}', flush=True)

    def stop(self, bot_uuid):
        with self._lock:
            entry = self._bots.pop(bot_uuid, None)
        if not entry:
            return ''
        entry.stop_event.set()
        entry.thread.join(timeout=5)
        return entry.client.updates_buf

    def send(self, bot_uuid, ilink_user_id, text, context_token=''):
        with self._lock:
            entry = self._bots.get(bot_uuid)
        if not entry:
            raise KeyError(f'bot {bot_uuid} not running')
        entry.client.send_text(ilink_user_id, text, context_token=context_token)

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
                self.manager.send(body['bot_uuid'], body['ilink_user_id'],
                                  body['text'], body.get('context_token', ''))
                self._reply(200, {'sent': True})
            else:
                self._reply(404, {'error': 'not found'})
        except KeyError as exc:
            self._reply(400, {'error': f'missing field: {exc}'})
        except Exception as exc:
            self._reply(500, {'error': str(exc)})


def serve(listen, grace_seconds=10.0):
    PollerHandler.manager = BotManager()
    server = ThreadingHTTPServer(listen, PollerHandler)
    print(f'bot_poller listening on {listen}', flush=True)

    stop = threading.Event()

    def _handle_signal(signum, frame):
        stop.set()

    import signal
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(sig, _handle_signal)
        except Exception:
            pass

    try:
        while not stop.is_set():
            stop.wait(0.5)
    finally:
        server.shutdown()


def main(argv=None):
    parser = argparse.ArgumentParser(description='GenericAgent Bot Poller')
    parser.add_argument('--listen', default=os.environ.get('BOT_POLLER_LISTEN', '127.0.0.1:8090'))
    parser.add_argument('--grace-seconds', type=float, default=10.0)
    args = parser.parse_args(argv)
    serve(args.listen, args.grace_seconds)


if __name__ == '__main__':
    main()
