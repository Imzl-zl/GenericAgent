"""WeChat iLink Bot protocol client.

Extracted from wechatapp.py so both the GA Core single-user entrypoint
(wechatapp.py) and the multi-tenant platform Bot Poller can share one
verified implementation of the iLink protocol: getupdates long-poll,
send text/media, CDN upload/download with AES encryption.

The optional `persist` flag controls local-file token/updates_buf persistence.
GA Core sets persist=True (writes ~/.wxbot/token.json); the platform Poller
sets persist=False and reads client.updates_buf back via the HTTP API.
"""

import os, sys, json, struct, base64, uuid, hashlib, time
from pathlib import Path
from urllib.parse import quote
import requests
from Crypto.Cipher import AES

# ── AuthExpired (errcode -14 from getUpdates) ──
class AuthExpired(Exception):
    """Bot token expired or invalid (errcode=-14)."""
    pass

API = 'https://ilinkai.weixin.qq.com'
TOKEN_FILE = Path.home() / '.wxbot' / 'token.json'
TOKEN_FILE.parent.mkdir(exist_ok=True)
VER, MSG_USER, MSG_BOT, ITEM_TEXT, STATE_FINISH = '2.1.10', 1, 2, 1, 2
ILINK_APP_ID = 'bot'
ILINK_APP_CLIENT_VERSION = (2 << 16) | (1 << 8) | 10
UA = f'openclaw-weixin/{VER}'
ITEM_IMAGE, ITEM_FILE, ITEM_VIDEO = 2, 4, 5
CDN_BASE = 'https://novac2c.cdn.weixin.qq.com/c2c'

# Avoid inherited proxy breaking WeChat long-poll SSL.
for _k in ('HTTPS_PROXY', 'https_proxy'):
    os.environ.pop(_k, None)


def _uin():
    return base64.b64encode(str(struct.unpack('>I', os.urandom(4))[0]).encode()).decode()


class WxBotClient:
    """Minimal iLink Bot HTTP client: login QR, getupdates, send text/media."""

    def __init__(self, token=None, token_file=None, persist=True, base_url=None):
        self._persist = persist
        self._tf = Path(token_file) if token_file else (TOKEN_FILE if persist else None)
        self.token = token
        self.bot_id = None
        self._buf = ''
        self._api = (base_url or API).rstrip('/')
        if not self.token and self._tf:
            self._load()

    # ── persistence (skipped when persist=False) ──
    def _load(self):
        if not self._tf or not self._tf.exists():
            return
        d = json.loads(self._tf.read_text('utf-8'))
        self.token = d.get('bot_token', '')
        self.bot_id = d.get('ilink_bot_id', '')
        self._buf = d.get('updates_buf', '')

    def _save(self, **kw):
        if not self._persist or not self._tf:
            return
        d = {'bot_token': self.token or '', 'ilink_bot_id': self.bot_id or '',
             'updates_buf': self._buf or '', **kw}
        self._tf.write_text(json.dumps(d, ensure_ascii=False, indent=2), 'utf-8')

    @property
    def updates_buf(self):
        return self._buf

    @updates_buf.setter
    def updates_buf(self, v):
        self._buf = v or ''

    # ── HTTP ──
    def _post(self, ep, body, timeout=15):
        data = json.dumps(body, ensure_ascii=False, separators=(',', ':')).encode('utf-8')
        h = {'Content-Type': 'application/json', 'AuthorizationType': 'ilink_bot_token',
             'Content-Length': str(len(data)), 'X-WECHAT-UIN': _uin(),
             'iLink-App-Id': ILINK_APP_ID,
             'iLink-App-ClientVersion': str(ILINK_APP_CLIENT_VERSION),
             'User-Agent': UA}
        tok = (self.token or '').strip()
        if tok:
            h['Authorization'] = f'Bearer {tok}'
        r = requests.post(f'{self._api}/{ep}', data=data, headers=h, timeout=timeout)
        r.raise_for_status()
        return r.json()

    # ── QR login (used by GA Core; platform Poller does binding in Go) ──
    def login_qr(self, poll_interval=2):
        d = {}
        for attempt in range(6):
            try:
                r = requests.get(f'{self._api}/ilink/bot/get_bot_qrcode',
                                 params={'bot_type': 3}, headers={'User-Agent': UA}, timeout=10)
                r.raise_for_status()
                d = r.json()
            except requests.exceptions.RequestException as e:
                print(f'[QR登录] 获取二维码失败（{e}），{2 ** attempt}s 后重试...')
                time.sleep(2 ** attempt)
                continue
            if d.get('qrcode') and d.get('qrcode_img_content'):
                break
            print(f'[QR登录] 二维码未就绪（可能被限流，ret={d.get("ret")}），{2 ** attempt}s 后重试...')
            time.sleep(2 ** attempt)
        if not (d.get('qrcode') and d.get('qrcode_img_content')):
            raise RuntimeError('多次重试仍未获取到可扫二维码（疑似限流），请稍后重试')
        qr_id, url = d['qrcode'], d.get('qrcode_img_content', '')
        print(f'[QR登录] ID: {qr_id}')
        if url:
            import qrcode
            qr = qrcode.QRCode(border=1); qr.add_data(url); qr.make(fit=True); qr.print_ascii(invert=True)
            try:
                qrcode.make(url).save(str(self._tf.parent / 'wx_qr.png'))
            except Exception as e:
                print(f'[QR登录] PNG 兜底保存失败（{e}），用上方 ASCII 二维码扫码即可')
        last = ''
        while True:
            time.sleep(poll_interval)
            try:
                s = requests.get(f'{self._api}/ilink/bot/get_qrcode_status',
                                 params={'qrcode': qr_id}, headers={'User-Agent': UA}, timeout=60).json()
            except (requests.exceptions.RequestException, ValueError):
                continue
            st = s.get('status', '')
            if st != last:
                print(f'  状态: {st}'); last = st
            if st == 'confirmed':
                self.token, self.bot_id = s.get('bot_token', ''), s.get('ilink_bot_id', '')
                self._save(login_time=time.strftime('%Y-%m-%d %H:%M:%S'))
                print(f'[QR登录] 成功! bot_id={self.bot_id}')
                return s
            if st == 'expired':
                raise RuntimeError('二维码过期')

    # ── receive ──
    def get_updates(self, timeout=30, request_timeout=None):
        try:
            http_timeout = timeout + 5 if request_timeout is None else max(0.05, request_timeout)
            resp = self._post('ilink/bot/getupdates',
                              {'get_updates_buf': self._buf or '',
                               'base_info': {'channel_version': VER}},
                              timeout=http_timeout)
        except requests.exceptions.ReadTimeout:
            return []
        if resp.get('errcode'):
            print(f'[getUpdates] err: {resp.get("errcode")} {resp.get("errmsg", "")}')
            if resp['errcode'] == -14:
                self._buf = ''; self.token = ''; self.bot_id = ''
                self._save(bot_token='', ilink_bot_id='')
                raise AuthExpired(resp.get('errmsg', ''))
            return []
        nb = resp.get('get_updates_buf', '')
        if nb:
            self._buf = nb; self._save()
        return resp.get('msgs') or []

    # ── send text ──
    def send_text(self, to_user_id, text, context_token=''):
        msg = {'from_user_id': '', 'to_user_id': to_user_id,
               'client_id': f'pyclient-{uuid.uuid4().hex[:16]}',
               'message_type': MSG_BOT, 'message_state': STATE_FINISH,
               'item_list': [{'type': ITEM_TEXT, 'text_item': {'text': text}}]}
        if context_token:
            msg['context_token'] = context_token
        return self._post('ilink/bot/sendmessage', {'msg': msg, 'base_info': {'channel_version': VER}})

    def send_typing(self, to_user_id, typing_ticket='', cancel=False):
        return self._post('ilink/bot/sendtyping', {
            'ilink_user_id': to_user_id, 'typing_ticket': typing_ticket,
            'status': 2 if cancel else 1,
            'base_info': {'channel_version': VER}})

    def get_typing_ticket(self, to_user_id, context_token=''):
        payload = {'ilink_user_id': to_user_id}
        if context_token:
            payload['context_token'] = context_token
        return self._post('ilink/bot/getconfig', payload).get('typing_ticket', '')

    # ── send media ──
    def _enc(self, raw, aes_key):
        pad = 16 - (len(raw) % 16)
        return AES.new(aes_key, AES.MODE_ECB).encrypt(raw + bytes([pad] * pad))

    def _upload(self, filekey, upload_param, raw, aes_key, timeout=120, upload_url=''):
        url = upload_url.strip() if upload_url else f'{CDN_BASE}/upload?encrypted_query_param={quote(upload_param)}&filekey={filekey}'
        data = self._enc(raw, aes_key)
        last_err = None
        for attempt in range(1, 4):
            try:
                r = requests.post(url, data=data, headers={'Content-Type': 'application/octet-stream', 'User-Agent': UA}, timeout=timeout)
                if 400 <= r.status_code < 500:
                    raise RuntimeError(f'CDN upload client error {r.status_code}: {r.headers.get("x-error-message") or r.text[:300]}')
                if r.status_code != 200:
                    raise RuntimeError(f'CDN upload server error: {r.headers.get("x-error-message") or f"status {r.status_code}"}')
                eq = r.headers.get('x-encrypted-param', '')
                if not eq:
                    raise RuntimeError('CDN upload response missing x-encrypted-param header')
                return {'encrypt_query_param': eq,
                        'aes_key': base64.b64encode(aes_key.hex().encode()).decode(), 'encrypt_type': 1}
            except Exception as e:
                last_err = e
                if 'client error' in str(e) or attempt >= 3:
                    break
                print(f'[WX] CDN upload retry {attempt}: {e}', file=sys.__stdout__)
        raise last_err

    def _build_upload_body(self, fp, raw, aes_key, item_key, ciphertext_size, thumb_raw, thumb_ciphertext_size):
        body = {
            'filekey': uuid.uuid4().hex, 'media_type': 0, 'to_user_id': '',
            'rawsize': len(raw), 'rawfilemd5': hashlib.md5(raw).hexdigest(),
            'filesize': ciphertext_size,
            'no_need_thumb': item_key not in ('image_item', 'video_item'),
            'aeskey': aes_key.hex(), 'base_info': {'channel_version': VER}}
        if thumb_raw:
            body.update({'thumb_rawsize': len(thumb_raw),
                         'thumb_rawfilemd5': hashlib.md5(thumb_raw).hexdigest(),
                         'thumb_filesize': thumb_ciphertext_size})
        return body

    def _make_thumbnail(self, fp):
        from io import BytesIO
        from PIL import Image
        im = Image.open(fp); im.thumbnail((240, 240))
        thumb_w, thumb_h = im.size
        if im.mode not in ('RGB', 'L'):
            im = im.convert('RGB')
        bio = BytesIO(); im.save(bio, format='JPEG', quality=85)
        return bio.getvalue(), thumb_w, thumb_h

    def _make_media_item(self, item_key, media, resp, ciphertext_size, thumb_w, thumb_h, thumb_size, fp):
        item = {'media': media}
        if item_key == 'file_item':
            item.update({'file_name': fp.name, 'len': str(len(fp.read_bytes()))})
        elif item_key == 'image_item':
            thumb_media = self._upload_thumb(resp, media)
            item.update({'mid_size': ciphertext_size, 'thumb_media': thumb_media,
                         'thumb_size': thumb_size, 'thumb_width': thumb_w, 'thumb_height': thumb_h})
        elif item_key == 'video_item':
            item.update({'video_size': ciphertext_size})
        return item

    def _upload_thumb(self, resp, fallback_media):
        thumb_param = resp.get('thumb_upload_param', '')
        thumb_url = resp.get('thumb_upload_full_url', '')
        if thumb_param or thumb_url:
            return self._upload('', thumb_param, b'', b'\x00' * 16, upload_url=thumb_url)
        return fallback_media

    def _send_media(self, to_user_id, file_path, media_type, item_type, item_key, context_token=''):
        fp = Path(file_path)
        raw = fp.read_bytes()
        aes_key = os.urandom(16)
        ciphertext_size = ((len(raw) // 16) + 1) * 16
        thumb_raw = b''; thumb_w = thumb_h = 0; thumb_ciphertext_size = 0
        if item_key == 'image_item':
            thumb_raw, thumb_w, thumb_h = self._make_thumbnail(fp)
            thumb_ciphertext_size = ((len(thumb_raw) // 16) + 1) * 16
        body = self._build_upload_body(fp, raw, aes_key, item_key, ciphertext_size, thumb_raw, thumb_ciphertext_size)
        body['media_type'] = media_type
        body['to_user_id'] = to_user_id
        resp = self._post('ilink/bot/getuploadurl', body)
        upload_param = resp.get('upload_param', '')
        upload_url = resp.get('upload_full_url', '')
        if not (upload_param or upload_url):
            raise RuntimeError(f'getuploadurl failed: {resp}')
        media = self._upload(body['filekey'], upload_param, raw, aes_key=aes_key, upload_url=upload_url)
        item = self._make_media_item(item_key, media, resp, ciphertext_size, thumb_w, thumb_h, thumb_ciphertext_size, fp)
        msg = {'from_user_id': '', 'to_user_id': to_user_id,
               'client_id': f'pyclient-{uuid.uuid4().hex[:16]}',
               'message_type': MSG_BOT, 'message_state': STATE_FINISH,
               'item_list': [{'type': item_type, item_key: item}]}
        if context_token:
            msg['context_token'] = context_token
        return self._post('ilink/bot/sendmessage', {'msg': msg, 'base_info': {'channel_version': VER}})

    def send_file(self, to_user_id, file_path, context_token=''):
        return self._send_media(to_user_id, file_path, 3, ITEM_FILE, 'file_item', context_token)

    def send_image(self, to_user_id, file_path, context_token=''):
        return self._send_media(to_user_id, file_path, 1, ITEM_IMAGE, 'image_item', context_token)

    def send_video(self, to_user_id, file_path, context_token=''):
        return self._send_media(to_user_id, file_path, 2, ITEM_VIDEO, 'video_item', context_token)

    @staticmethod
    def extract_text(msg):
        return '\n'.join(it['text_item'].get('text', '')
                         for it in msg.get('item_list', [])
                         if it.get('type') == ITEM_TEXT and it.get('text_item'))

    @staticmethod
    def is_user_msg(msg):
        return msg.get('message_type') == MSG_USER

    def run_loop(self, on_message, poll_timeout=30):
        print(f'[Bot] 监听中... (bot_id={self.bot_id})')
        seen = set()
        while True:
            try:
                for msg in self.get_updates(poll_timeout):
                    mid = msg.get('message_id', 0)
                    if not self.is_user_msg(msg) or mid in seen:
                        continue
                    seen.add(mid)
                    if len(seen) > 5000:
                        seen = set(list(seen)[-2000:])
                    try:
                        on_message(self, msg)
                    except Exception as e:
                        print(f'[Bot] 回调异常: {e}')
            except KeyboardInterrupt:
                print('[Bot] 退出'); break
            except AuthExpired:
                raise
            except Exception as e:
                print(f'[Bot] 异常: {e}，5s重试'); time.sleep(5)
