import asyncio, json, os, sys, threading, time
import requests

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
_TEMP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), 'temp')
from agentmain import GeneraticAgent
from chatapp_common import (AgentChatMixin, build_done_text, clean_reply, ensure_single_instance,
                            public_access, redirect_log, require_runtime,
                            resolve_file_markers, split_text, strip_files)
from llmcore import mykeys

try:
    from dingtalk_stream import AckMessage, CallbackHandler, Credential, DingTalkStreamClient
    from dingtalk_stream.chatbot import ChatbotMessage
except Exception:
    print("Please install dingtalk-stream to use DingTalk: pip install dingtalk-stream")
    sys.exit(1)

agent = GeneraticAgent(); agent.verbose = False
CLIENT_ID = str(mykeys.get("dingtalk_client_id", "") or "").strip()
CLIENT_SECRET = str(mykeys.get("dingtalk_client_secret", "") or "").strip()
ALLOWED = {str(x).strip() for x in mykeys.get("dingtalk_allowed_users", []) if str(x).strip()}
USER_TASKS = {}


class DingTalkApp(AgentChatMixin):
    label, source, split_limit = "DingTalk", "dingtalk", 1800

    def __init__(self):
        super().__init__(agent, USER_TASKS)
        self.client, self.access_token, self.token_expiry, self.background_tasks = None, None, 0, set()

    async def _get_access_token(self):
        if self.access_token and time.time() < self.token_expiry:
            return self.access_token

        def _fetch():
            resp = requests.post("https://api.dingtalk.com/v1.0/oauth2/accessToken", json={"appKey": CLIENT_ID, "appSecret": CLIENT_SECRET}, timeout=20)
            resp.raise_for_status()
            return resp.json()

        last_err = None
        for attempt in range(2):
            try:
                data = await asyncio.to_thread(_fetch)
                self.access_token = data.get("accessToken")
                self.token_expiry = time.time() + int(data.get("expireIn", 7200)) - 60
                return self.access_token
            except Exception as e:
                last_err = e
                if attempt == 0:
                    await asyncio.sleep(1)
        print(f"[DingTalk] token error after retry: {last_err}")
        return None

    async def _send_batch_message(self, chat_id, msg_key, msg_param):
        token = await self._get_access_token()
        if not token:
            return False
        headers = {"x-acs-dingtalk-access-token": token}
        if chat_id.startswith("group:"):
            url = "https://api.dingtalk.com/v1.0/robot/groupMessages/send"
            payload = {"robotCode": CLIENT_ID, "openConversationId": chat_id[6:], "msgKey": msg_key, "msgParam": json.dumps(msg_param, ensure_ascii=False)}
        else:
            url = "https://api.dingtalk.com/v1.0/robot/oToMessages/batchSend"
            payload = {"robotCode": CLIENT_ID, "userIds": [chat_id], "msgKey": msg_key, "msgParam": json.dumps(msg_param, ensure_ascii=False)}

        def _post():
            resp = requests.post(url, json=payload, headers=headers, timeout=20)
            body = resp.text
            if resp.status_code != 200:
                raise RuntimeError(f"HTTP {resp.status_code}: {body[:300]}")
            result = resp.json() if "json" in resp.headers.get("content-type", "") else {}
            errcode = result.get("errcode")
            if errcode not in (None, 0):
                raise RuntimeError(f"API errcode={errcode}: {body[:300]}")
            return True

        try:
            return await asyncio.to_thread(_post)
        except Exception as e:
            print(f"[DingTalk] send error: {e}")
            return False

    async def send_text(self, chat_id, content):
        for part in split_text(content, self.split_limit):
            await self._send_batch_message(chat_id, "sampleMarkdown", {"text": part, "title": "Agent Reply"})

    # -- 出站媒体(2026-08-14 独立审查 C1) --------------------------------
    # 图片: oapi/media/upload(企业内部应用)→ media_id → sampleImageMsg。
    # 与平台 poller DingTalkAdapter 同构(官方契约: 图片仅 jpg/gif/png/bmp
    # ≤20MB); 文件/视频的 downloadCode 上传流未实现 → fail-closed 诚实提示
    # (不静默降级、不输出服务器路径)。
    _IMAGE_EXTS = {'.jpg', '.jpeg', '.png', '.gif', '.bmp'}
    _IMAGE_MAX_BYTES = 20 * 1024 * 1024

    async def _upload_media(self, file_path, media_type):
        """钉钉媒体上传(oapi/media/upload) → media_id。"""
        token = await self._get_access_token()
        if not token:
            raise RuntimeError('dingtalk access token failed')

        def _post():
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

        return await asyncio.to_thread(_post)

    async def send_done(self, chat_id, raw_text, **ctx):
        files = resolve_file_markers(raw_text, base_dir=_TEMP_DIR)
        if not files:
            return await self.send_text(chat_id, build_done_text(raw_text), **ctx)
        body = strip_files(clean_reply(raw_text))
        if body and body != "...":
            await self.send_text(chat_id, body, **ctx)
        unsent = []
        for fp in files:
            ext = os.path.splitext(fp)[1].lower()
            if ext not in self._IMAGE_EXTS:
                unsent.append(fp)
                continue
            try:
                if os.path.getsize(fp) > self._IMAGE_MAX_BYTES:
                    raise RuntimeError(f'image exceeds dingtalk 20MB limit: {os.path.getsize(fp)}')
                media_id = await self._upload_media(fp, 'image')
                ok = await self._send_batch_message(chat_id, 'sampleImageMsg', {'photo': media_id})
                if not ok:
                    unsent.append(fp)
            except Exception as e:
                print(f'[DingTalk] send image error: {e}')
                unsent.append(fp)
        if unsent:
            await self.send_text(
                chat_id,
                "📎 部分文件已生成，但当前渠道仅支持图片直发（文件/视频请在服务端 outputs/ 查看）。")


    async def on_message(self, content, sender_id, sender_name, conversation_type=None, conversation_id=None):
        try:
            if not content:
                return
            if not public_access(ALLOWED) and sender_id not in ALLOWED:
                print(f"[DingTalk] unauthorized user: {sender_id}")
                return
            is_group = conversation_type == "2" and conversation_id
            chat_id = f"group:{conversation_id}" if is_group else sender_id
            print(f"[DingTalk] message from {sender_name} ({sender_id}): {content}")
            if content.startswith("/"):
                return await self.handle_command(chat_id, content)
            task = asyncio.create_task(self.run_agent(chat_id, content))
            self.background_tasks.add(task)
            task.add_done_callback(self.background_tasks.discard)
        except Exception:
            import traceback
            print("[DingTalk] handle_message error")
            traceback.print_exc()

    async def start(self):
        self.client = DingTalkStreamClient(Credential(CLIENT_ID, CLIENT_SECRET))
        self.client.register_callback_handler(ChatbotMessage.TOPIC, _DingTalkHandler(self))
        print("[DingTalk] bot starting...")
        delay, max_delay = 5, 300
        while True:
            started_at = time.monotonic()
            try:
                await self.client.start()
            except Exception as e:
                print(f"[DingTalk] stream error: {e}")
            # any session that lived >=60s is treated as healthy -> reset backoff
            if time.monotonic() - started_at >= 60:
                delay = 5
            print(f"[DingTalk] reconnect in {delay}s...")
            await asyncio.sleep(delay)
            delay = min(delay * 2, max_delay)


class _DingTalkHandler(CallbackHandler):
    def __init__(self, app):
        super().__init__()
        self.app = app

    async def process(self, message):
        try:
            chatbot_msg = ChatbotMessage.from_dict(message.data)
            text = getattr(getattr(chatbot_msg, "text", None), "content", "") or ""
            extensions = getattr(chatbot_msg, "extensions", None) or {}
            recognition = ((extensions.get("content") or {}).get("recognition") or "").strip() if isinstance(extensions, dict) else ""
            if not (text := text.strip()):
                text = recognition or str((message.data.get("text", {}) or {}).get("content", "") or "").strip()
            sender_id = str(getattr(chatbot_msg, "sender_staff_id", None) or getattr(chatbot_msg, "sender_id", None) or "unknown")
            sender_name = getattr(chatbot_msg, "sender_nick", None) or "Unknown"
            await self.app.on_message(text, sender_id, sender_name, message.data.get("conversationType"), message.data.get("conversationId") or message.data.get("openConversationId"))
        except Exception as e:
            print(f"[DingTalk] callback error: {e}")
        return AckMessage.STATUS_OK, "OK"


if __name__ == "__main__":
    _LOCK_SOCK = ensure_single_instance(19530, "DingTalk")
    require_runtime(agent, "DingTalk", dingtalk_client_id=CLIENT_ID, dingtalk_client_secret=CLIENT_SECRET)
    redirect_log(__file__, "dingtalkapp.log", "DingTalk", ALLOWED)
    threading.Thread(target=agent.run, daemon=True).start()
    asyncio.run(DingTalkApp().start())
