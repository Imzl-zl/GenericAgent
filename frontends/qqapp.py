import asyncio, os, sys, threading, time
from collections import deque

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from agentmain import GeneraticAgent
from chatapp_common import (AgentChatMixin, build_done_text, ensure_single_instance,
                            public_access, redirect_log, require_runtime, split_text)
from im_markers import resolve_file_markers
from llmcore import mykeys
from qq_media import QQMediaSender, QQ_MEDIA_FILE, QQ_MEDIA_IMAGE, QQ_MEDIA_VIDEO

try:
    import botpy
    from botpy.message import C2CMessage, GroupMessage
except Exception:
    print("Please install qq-botpy to use QQ module: pip install qq-botpy")
    sys.exit(1)

agent = GeneraticAgent(); agent.verbose = False
# 媒体交付(2026-08-16 升级): 本前端由纯文本通道升级为媒体通道——QQ 官方
# 无一步式发图接口, 整文件上传需公网 URL(根路径无公网入口)或官方
# rich-media 分片 4 步流程; 实现见 qq_media.QQMediaSender(与平台 bot_poller
# QQAdapter 同构, 该实现 2026-08-14 审查 B2/I4 已过审)。生图/文件产出经
# send_done 解析 [FILE:] marker 后分片上传直发; 失败回退文本提示(内容不丢)。
# 残余风险: 无真实凭据回归自动化, 参数细节以实际使用验证为准(平台同款)。
APP_ID = str(mykeys.get("qq_app_id", "") or "").strip()
APP_SECRET = str(mykeys.get("qq_app_secret", "") or "").strip()
ALLOWED = {str(x).strip() for x in mykeys.get("qq_allowed_users", []) if str(x).strip()}
PROCESSED_IDS, USER_TASKS = deque(maxlen=1000), {}
SEQ_LOCK, MSG_SEQ = threading.Lock(), 1


def _next_msg_seq():
    global MSG_SEQ
    with SEQ_LOCK:
        MSG_SEQ += 1
        return MSG_SEQ


def _build_intents():
    # qq-botpy>=1.0(pyproject 约束): public_messages(1<<25) 同时驱动
    # on_group_at_message_create 与 on_c2c_message_create(群@ + 单聊)。
    # 旧属性名(c2c_message/group_at_message 等)在 1.x 不存在, 不再探测。
    try:
        return botpy.Intents(public_messages=True)
    except Exception:
        intents = botpy.Intents.none() if hasattr(botpy.Intents, "none") else botpy.Intents()
        for attr in ("public_messages", "public_guild_messages", "direct_message"):
            if hasattr(intents, attr):
                try:
                    setattr(intents, attr, True)
                except Exception:
                    pass
        return intents


def _make_bot_class(app):
    class QQBot(botpy.Client):
        def __init__(self):
            super().__init__(intents=_build_intents(), ext_handlers=False)

        async def on_ready(self):
            print(f"[QQ] bot ready: {getattr(getattr(self, 'robot', None), 'name', 'QQBot')}")

        async def on_c2c_message_create(self, message: C2CMessage):
            await app.on_message(message, is_group=False)

        async def on_group_at_message_create(self, message: GroupMessage):
            await app.on_message(message, is_group=True)
        # 无 on_direct_message_create: 频道私信(direct_message intent 1<<12)
        # 未订阅——QQ 开放平台机器人目标面是群@ + C2C 单聊(public_messages
        # 1<<25 覆盖), 旧代码订阅 direct_message 但 handler 从未生效, 死代码
        # 已清理; 如需频道私信需另申请频道权限并订阅该 intent。

    return QQBot


class QQApp(AgentChatMixin):
    label, source, split_limit = "QQ", "qq", 1500
    #: 媒体直发(2026-08-16 升级, 官方 rich-media 分片上传)。
    can_send_media = True
    #: [FILE:] marker 相对路径解析根(与 wechatapp/tgapp 同款)。
    _TEMP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), 'temp')
    #: 扩展名 → 官方 media file_type(1=图片 2=视频 4=文件)。
    _IMG_EXTS = {'.jpg', '.jpeg', '.png', '.gif', '.webp', '.bmp'}
    _VIDEO_EXTS = {'.mp4', '.mov', '.m4v', '.webm'}

    def __init__(self):
        super().__init__(agent, USER_TASKS)
        self.client = None
        #: 媒体发送器单例(2026-08-16 复审: 每次 send_done 新建会重置令牌
        #: 桶, 15 QPM 频控形同虚设——必须按 app 生命周期持有)。
        self._media_sender = None

    def _media(self):
        if self._media_sender is None and self.client is not None:
            self._media_sender = QQMediaSender(self.client)
        return self._media_sender

    async def send_done(self, chat_id, raw_text, **ctx):
        """终态交付: 文本照常 send_text; [FILE:] 产出走分片上传直发
        (图片/视频/文件), 失败回退一句提示(内容不丢, 同微信语义)。"""
        files = resolve_file_markers(raw_text, base_dir=self._TEMP_DIR)
        text = build_done_text(raw_text)
        if text != "..." or not files:
            await self.send_text(chat_id, text, **ctx)
        if not files:
            return
        sender = self._media()
        if sender is None:
            # 渠道未就绪(极端): 不静默丢文件, 明确告知。
            await self.send_text(chat_id, "📎 文件已生成，但 QQ 通道尚未就绪，无法直发。", **ctx)
            return
        is_group = bool(ctx.get('is_group'))
        for fpath in files:
            ext = os.path.splitext(fpath)[1].lower()
            ftype = (QQ_MEDIA_IMAGE if ext in self._IMG_EXTS else
                     QQ_MEDIA_VIDEO if ext in self._VIDEO_EXTS else
                     QQ_MEDIA_FILE)
            try:
                await sender.send_media(chat_id, fpath, file_type=ftype,
                                        is_group=is_group)
                print(f'[QQ] sent media: {fpath}', file=sys.__stdout__)
            except Exception as e:
                print(f'[QQ] media send err: {fpath}: {e}', file=sys.__stdout__)
                await self.send_text(chat_id, f"📎 文件已生成但发送失败: {e}", **ctx)

    async def send_text(self, chat_id, content, *, msg_id=None, is_group=False):
        if not self.client:
            return
        api = self.client.api.post_group_message if is_group else self.client.api.post_c2c_message
        key = "group_openid" if is_group else "openid"
        for part in split_text(content, self.split_limit):
            seq = _next_msg_seq()
            try:
                await api(**{key: chat_id, "msg_type": 2, "markdown": {"content": part}, "msg_id": msg_id, "msg_seq": seq})
            except Exception:
                await api(**{key: chat_id, "msg_type": 0, "content": part, "msg_id": msg_id, "msg_seq": seq})

    async def on_message(self, data, is_group=False):
        try:
            msg_id = getattr(data, "id", None)
            if msg_id in PROCESSED_IDS:
                return
            PROCESSED_IDS.append(msg_id)
            content = (getattr(data, "content", "") or "").strip()
            if not content:
                return
            author = getattr(data, "author", None)
            user_id = str(getattr(author, "member_openid" if is_group else "user_openid", "") or getattr(author, "id", "") or "unknown")
            chat_id = str(getattr(data, "group_openid", "") or user_id) if is_group else user_id
            if not public_access(ALLOWED) and user_id not in ALLOWED:
                print(f"[QQ] unauthorized user: {user_id}")
                return
            print(f"[QQ] message from {user_id} ({'group' if is_group else 'c2c'}): {content}")
            if content.startswith("/"):
                return await self.handle_command(chat_id, content, msg_id=msg_id, is_group=is_group)
            asyncio.create_task(self.run_agent(chat_id, content, msg_id=msg_id, is_group=is_group))
        except Exception:
            import traceback
            print("[QQ] handle_message error")
            traceback.print_exc()

    async def start(self):
        self.client = _make_bot_class(self)()
        delay, max_delay = 5, 300
        while True:
            started_at = time.monotonic()
            try:
                print(f"[QQ] bot starting... {time.strftime('%m-%d %H:%M')}")
                await self.client.start(appid=APP_ID, secret=APP_SECRET)
            except Exception as e:
                print(f"[QQ] bot error: {e}")
            if time.monotonic() - started_at >= 60:
                delay = 5
            print(f"[QQ] reconnect in {delay}s...")
            await asyncio.sleep(delay)
            delay = min(delay * 2, max_delay)


if __name__ == "__main__":
    _LOCK_SOCK = ensure_single_instance(19528, "QQ")
    require_runtime(agent, "QQ", qq_app_id=APP_ID, qq_app_secret=APP_SECRET)
    redirect_log(__file__, "qqapp.log", "QQ", ALLOWED)
    threading.Thread(target=agent.run, daemon=True).start()
    asyncio.run(QQApp().start())
