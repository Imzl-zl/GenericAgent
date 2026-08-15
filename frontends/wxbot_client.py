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


def fit_image_for_upload(file_path, *, max_bytes=None, allowed_formats=None, animated_ok=True):
    """渠道媒体档案适配(2026-08-14 官方文档对齐, 微信/企微/钉钉共用):
    - allowed_formats: PIL 格式白名单(官方文档: 企微图片仅 JPG/PNG; 钉钉
      jpg/gif/png/bmp 无 webp)。白名单外的静态图转 JPEG; 白名单外的动图
      GIF(animated_ok=False, 企微)取首帧转静态 JPEG。
    - max_bytes: 超限迭代压缩(质量 q90→55, 再降采样 0.8^N)到 ≤max_bytes。
    - animated_ok=True(微信/钉钉): 动图一律保留不转(丢动画不可接受)。
    返回临时文件 Path(调用方负责清理)或 None(无需适配/非图片/失败回退原图)。
    临时文件用 mkstemp 落可写系统临时目录——生产 poller 只读 rootfs, 源
    (spool)卷只读, 写源旁目录会静默失败(2026-08-14 生产实证)。"""
    fp = Path(file_path)
    try:
        from PIL import Image
    except ImportError:
        # 硬依赖缺失必须显式告警(2026-08-16 复审): 此前惰性导入 + 全量
        # except 吞掉 ModuleNotFoundError, 缺 pillow 时静默回退原图, 让
        # 2026-08-14 CDN 超限事故无声复发。回退语义保留(调用方仍走原图),
        # 但缺失原因必须可见。
        print('[wxbot_client] PIL missing (pip install pillow): image fit disabled, '
              'oversized uploads will be sent raw', file=sys.stderr)
        return None
    try:
        with Image.open(fp) as im:
            im.load()
            fmt = (im.format or '').upper()
            animated = bool(getattr(im, 'is_animated', False))
    except Exception:
        return None
    try:
        size = fp.stat().st_size
    except OSError:
        return None
    if animated and animated_ok:
        return None  # 动图保留(丢动画不可接受)
    need_format = allowed_formats is not None and fmt not in allowed_formats
    need_size = max_bytes is not None and size > max_bytes
    if not need_format and not need_size:
        return None
    import tempfile
    from io import BytesIO
    out = None
    try:
        fd, tmp_name = tempfile.mkstemp(prefix='.fit_', suffix='.jpg')
        os.close(fd)
        out = Path(tmp_name)
        bio = BytesIO()
        target = max_bytes or (1 << 62)
        buf = None
        # 迭代压缩: q90→55 逐档降质, 仍超限再降采样(0.9 起每档 -0.15,
        # 共 5 档到 0.3 以下, 注释与实现一致, 2026-08-14 复审修正)。
        for quality in (90, 80, 70, 60, 55):
            bio.seek(0); bio.truncate()
            im.convert('RGB').save(bio, format='JPEG', quality=quality)
            if bio.tell() <= target:
                buf = bio.getvalue(); break
        if buf is None:
            scale, cur = 0.9, im
            while scale > 0.3:
                w, h = max(1, int(im.width * scale)), max(1, int(im.height * scale))
                cur = im.resize((w, h), Image.LANCZOS)
                bio.seek(0); bio.truncate()
                cur.convert('RGB').save(bio, format='JPEG', quality=70)
                if bio.tell() <= target:
                    buf = bio.getvalue(); break
                scale -= 0.15
        if buf is None:
            return None
        out.write_bytes(buf)
        return out
    except Exception:
        try:
            if out is not None:
                out.unlink(missing_ok=True)
        except OSError:
            pass
        return None


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
    def send_text(self, to_user_id, text, context_token='', client_id=None):
        # 审查 round9: client_id 由 Platform 传入稳定幂等键(delivery_id:part),
        # 重试投递同一内容时保持同 id, 供服务端去重; 空值回退随机(兼容旧调用方)。
        msg = {'from_user_id': '', 'to_user_id': to_user_id,
               'client_id': client_id or f'pyclient-{uuid.uuid4().hex[:16]}',
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

    def _upload(self, filekey, upload_param, raw, aes_key, upload_url=''):
        url = upload_url.strip() if upload_url else f'{CDN_BASE}/upload?encrypted_query_param={quote(upload_param)}&filekey={filekey}'
        data = self._enc(raw, aes_key)
        last_err = None
        # 2026-08-14 生产事故修复(微信生图交付死信)后的预算定稿(独立审查
        # 优化, 2026-08-14): CDN 瞬时故障(握手挂起/SSLEOFError)时, 单一
        # timeout 同时覆盖连接与传输, 连接层故障最长阻塞 timeout×3 次,
        # 远超 Go delivery /send 预算 → 8 次重试全部超时 → 死信。现在
        # 拆分为固定双段超时, 与 Go 侧预算链对齐:
        #   - connect=30s: urllib3 的 connect timeout 同时约束连接建立与
        #     请求体发送, 本服务器→微信 CDN 实测限速 ~18KB/s, 300KB≈14s,
        #     30s 写预算内留余量(旧 10s 写预算过紧, 部署后仍死信)。
        #   - read=10s: 响应读取(正常 <1s); 服务器收完 body 后挂起不回包
        #     = 故障, 10s 快速失败(旧 read=120s 会让单次尝试最长挂 150s,
        #     远超 Go 预算且占 poller 线程)。
        #   正常/快速失败路径: getuploadurl(≤10s, 控制面) + 30+10+3+30+10
        #   + sendmessage(≤10s) ≈ 85s < Go 媒体预算 90s(ctx); 病理(控制面
        #   与 CDN 双侧挂起)最坏 ~103s 超 ctx——由 Go 侧重试 + 幂等
        #   client_id 兜底, poller 线程占用有界(非死锁), 不引入新问题。
        #   失败快速上抛由 Go 侧整体重试(不无限阻塞 poller 线程)。
        connect_timeout, read_timeout = 30, 10
        for attempt in range(1, 3):
            try:
                r = requests.post(url, data=data,
                                  headers={'Content-Type': 'application/octet-stream', 'User-Agent': UA},
                                  timeout=(connect_timeout, read_timeout))
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
                if 'client error' in str(e) or attempt >= 2:
                    break
                time.sleep(3)
                print(f'[WX] CDN upload retry {attempt}: {e}', file=sys.__stdout__)
        raise last_err

    def _build_upload_body(self, fp, raw, aes_key, item_key, ciphertext_size, thumb_raw, thumb_ciphertext_size):
        # 2026-08-14 官方协议对齐: 腾讯 openclaw 实现默认 no_need_thumb=true
        # (protocol-spec §8.6: "只上传主文件, 不上传缩略图; 公开实现里图片
        # 消息往往只有 media, 没有 thumb_media")。旧实现 no_need_thumb=false
        # 且服务端返回 thumb_upload_param 时上传空字节当缩略图——偏离官方
        # 默认且是隐患。thumb_raw 参数保留兼容(调用方显式传了才要缩略图)。
        body = {
            'filekey': uuid.uuid4().hex, 'media_type': 0, 'to_user_id': '',
            'rawsize': len(raw), 'rawfilemd5': hashlib.md5(raw).hexdigest(),
            'filesize': ciphertext_size,
            'no_need_thumb': not thumb_raw,
            'aeskey': aes_key.hex(), 'base_info': {'channel_version': VER}}
        if thumb_raw:
            body.update({'thumb_rawsize': len(thumb_raw),
                         'thumb_rawfilemd5': hashlib.md5(thumb_raw).hexdigest(),
                         'thumb_filesize': thumb_ciphertext_size})
        return body

    def _make_media_item(self, item_key, media, resp, ciphertext_size, thumb_w, thumb_h, thumb_size, fp, file_name='', raw=b''):
        item = {'media': media}
        if item_key == 'file_item':
            # 审查 R5-I10: 优先使用显式显示名, 回退到本地文件名(兼容旧调用方)。
            # len 直接用已读入的 raw(2026-08-14 复审: 旧实现二次整读文件)。
            item.update({'file_name': file_name or fp.name, 'len': str(len(raw))})
        elif item_key == 'image_item':
            # 官方形态(openclaw no_need_thumb=true): media + mid_size。
            item.update({'mid_size': ciphertext_size})
            # 防御兼容(2026-08-14 复审门控): 仅当调用方显式传了真缩略图
            # (thumb_raw 非空)才可能触发——当前无调用方传, 生产不可达;
            # 旧条件 `if raw and ...` 在服务端返回 thumb_upload_param 时会把
            # 主图再上传一遍当缩略图(多一次 CDN 往返, 超预算)。
            if thumb_size and raw and (resp.get('thumb_upload_param') or resp.get('thumb_upload_full_url')):
                thumb_media = self._upload('', resp.get('thumb_upload_param', ''),
                                           raw, aes_key=b'\x00' * 16,
                                           upload_url=resp.get('thumb_upload_full_url', ''))
                item.update({'thumb_media': thumb_media, 'thumb_size': thumb_size,
                             'thumb_width': thumb_w, 'thumb_height': thumb_h})
        elif item_key == 'video_item':
            item.update({'video_size': ciphertext_size})
        return item

    def _send_media(self, to_user_id, file_path, media_type, item_type, item_key, context_token='', file_name='', client_id=None):
        fp = Path(file_path)
        raw = fp.read_bytes()
        aes_key = os.urandom(16)
        ciphertext_size = ((len(raw) // 16) + 1) * 16
        # 官方 openclaw 默认不上传缩略图(no_need_thumb=true); thumb 字段
        # 保留默认空(_build_upload_body 的 thumb_raw 参数为兼容显式调用方)。
        thumb_raw = b''; thumb_w = thumb_h = 0; thumb_ciphertext_size = 0
        body = self._build_upload_body(fp, raw, aes_key, item_key, ciphertext_size, thumb_raw, thumb_ciphertext_size)
        body['media_type'] = media_type
        body['to_user_id'] = to_user_id
        # 控制面调用显式小超时(2026-08-14 复审 P2): 正常 <1s, 与 CDN 段
        # read=10s 同一快速失败语义——否则病理最坏(控制面+CDN 双侧挂起)
        # = 15+83+15 ≈ 113s 超 Go ctx 90s(poller 线程占线由 Go 重试 + 
        # client_id 幂等兜底, 有界非死锁)。
        resp = self._post('ilink/bot/getuploadurl', body, timeout=(10, 10))
        upload_param = resp.get('upload_param', '')
        upload_url = resp.get('upload_full_url', '')
        if not (upload_param or upload_url):
            raise RuntimeError(f'getuploadurl failed: {resp}')
        media = self._upload(body['filekey'], upload_param, raw, aes_key=aes_key, upload_url=upload_url)
        # 审查 R5-I10: 用户可见文件名由 Platform 显式传入(file_name), 不从
        # 本地临时路径 basename 推导(快照文件名含 marker hash 前缀)。
        item = self._make_media_item(item_key, media, resp, ciphertext_size, thumb_w, thumb_h, thumb_ciphertext_size, fp, file_name=file_name, raw=raw)
        msg = {'from_user_id': '', 'to_user_id': to_user_id,
               'client_id': client_id or f'pyclient-{uuid.uuid4().hex[:16]}',
               'message_type': MSG_BOT, 'message_state': STATE_FINISH,
               'item_list': [{'type': item_type, item_key: item}]}
        if context_token:
            msg['context_token'] = context_token
        return self._post('ilink/bot/sendmessage', {'msg': msg, 'base_info': {'channel_version': VER}}, timeout=(10, 10))

    def send_file(self, to_user_id, file_path, context_token='', file_name='', client_id=None):
        return self._send_media(to_user_id, file_path, 3, ITEM_FILE, 'file_item', context_token, file_name=file_name, client_id=client_id)

    def send_image(self, to_user_id, file_path, context_token='', client_id=None):
        # 2026-08-14 生产事故修复(微信生图交付死信, 根因之二): 本服务器
        # (海外)→微信 C2C CDN 大文件上传被限速 ~20KB/s 且连接 ~30s 被杀,
        # >~500KB 的静态图上传必失败(1.3-1.6MB 的生成图全部 dead-letter)。
        # 交付侧透明适配: 超限静态图(png/jpeg/webp/bmp, 非动图)转 JPEG
        # 并按质量/尺寸迭代缩到 ≤300KB 再上传——IM 场景视觉无损, 用户无感。
        # GIF(动图)不转换(丢动画), 由生成端 size/质量控制。
        adapted = self._fit_static_image_for_upload(file_path)
        if adapted is None:
            return self._send_media(to_user_id, file_path, 1, ITEM_IMAGE, 'image_item', context_token, client_id=client_id)
        try:
            return self._send_media(to_user_id, str(adapted), 1, ITEM_IMAGE, 'image_item', context_token, client_id=client_id)
        finally:
            try:
                adapted.unlink(missing_ok=True)
            except OSError:
                pass

    #: 微信 CDN 上传安全上限(实测节流 ~18KB/s: 300KB≈14s, 400KB≈22s, 450KB≈22s,
    # 1MB≈30s 被杀; 30s 写预算内留余量)。
    _IMAGE_UPLOAD_MAX_BYTES = 300 * 1024

    def _fit_static_image_for_upload(self, file_path):
        """超限静态图 → 临时 JPEG(≤300KB), 返回 Path; 无需转换返回 None。
        委托模块级 fit_image_for_upload(微信档案: 只按大小, 动图不转)。
        失败(非图片/编码异常)返回 None, 由调用方走原图(错误语义不变)。"""
        return fit_image_for_upload(
            file_path, max_bytes=self._IMAGE_UPLOAD_MAX_BYTES, animated_ok=True)

    def send_video(self, to_user_id, file_path, context_token='', client_id=None):
        return self._send_media(to_user_id, file_path, 2, ITEM_VIDEO, 'video_item', context_token, client_id=client_id)

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
