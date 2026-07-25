import os, sys, re, threading, queue, time, socket
from pathlib import Path
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
_TEMP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), 'temp')
from agentmain import GeneraticAgent
from wxbot_client import WxBotClient, AuthExpired
from wxbot_media import download_media

# ── Per-user abort flags (shared between on_message invocations) ──
_task_aborted: dict = {}  # uid -> True  (set by /stop, read by _handle)

agent = GeneraticAgent()
agent.verbose = False

_TAG_PATS = [r'<' + t + r'>.*?</' + t + r'>' for t in ('thinking', 'tool_use')]
_TAG_PATS.append(r'<file_content>.*?</file_content>')

def _strip_md(t):
    """Filter markdown for WeChat rich-text rendering.
    WeChat natively renders: code fences, inline code, bold, italic,
    H1-H4 headings, horizontal rules, tables. We only strip unsupported syntax."""
    def _trunc_code(m):
        full = m.group()
        fence = re.match(r'`{3,}', full).group()
        rest = full[len(fence):-len(fence)]
        if '\n' not in rest: return full  # single-line, keep as-is
        lang_line, _, body = rest.partition('\n')
        lines = body.split('\n')
        if len(lines) > 10:
            return f'{fence}{lang_line}\n' + '\n'.join(lines[:10]) + '\n...\n' + fence
        return full  # keep intact
    t = re.sub(r'(`{3,})[\s\S]*?\1', _trunc_code, t)
    # inline code: keep (WeChat renders it)
    # bold/italic (*/**/***): keep (WeChat renders it)
    t = re.sub(r'!\[.*?\]\(.*?\)', '', t)                        # images: remove
    t = re.sub(r'\[([^\]]+)\]\([^\)]+\)', r'\1', t)              # links: text only
    t = re.sub(r'^#{5,6}\s+', '', t, flags=re.M)                 # H5-H6: strip (H1-H4 kept)
    t = re.sub(r'^\s*[-*+]\s+', '• ', t, flags=re.M)             # unordered list: bullet
    t = re.sub(r'^\s*\d+\.\s+', '', t, flags=re.M)               # ordered list: strip num
    t = re.sub(r'^\s*>\s?', '', t, flags=re.M)                   # blockquote: strip
    # horizontal rules (---): keep (WeChat renders it)
    return re.sub(r'\n{3,}', '\n\n', t).strip()

def _clean(t):
    t = re.sub(r'^\s*LLM Running \(Turn \d+\) \.{3}\s*$', '', t, flags=re.M)
    t = re.sub(r'^\s*🛠️\s*[A-Za-z_][A-Za-z0-9_]*\(.*$', '', t, flags=re.M)
    for p in _TAG_PATS:
        t = re.sub(p, '', t, flags=re.DOTALL)
    t = re.sub(r'</?summary>', '', t)
    return re.sub(r'\n{3,}', '\n\n', _strip_md(t)).strip()

def on_message(bot, msg):
    text = bot.extract_text(msg).strip()
    uid = msg.get('from_user_id', '')
    ctx = msg.get('context_token', '')
    media_paths = download_media(msg.get('item_list', []))
    if not text and not media_paths: return
    if media_paths:
        text = (text + '\n' if text else '') + '\n'.join(f'[用户发送文件: {p}]' for p in media_paths)
    print(f'[WX] 收到: {text[:80]}', file=sys.__stdout__)

    # Commands
    if text in ('/stop', '/abort'):
        agent.abort()
        _task_aborted[uid] = True
        print(f'[WX] /stop set _task_aborted[{uid}]', file=sys.__stdout__)
        return
    if text.startswith('/llm'):
        args = text.split()
        if len(args) > 1:
            try:
                n = int(args[1]); agent.next_llm(n)
                bot.send_text(uid, f'切换到 [{agent.llm_no}] {agent.get_llm_name()}', context_token=ctx)
            except (ValueError, IndexError):
                bot.send_text(uid, f'用法: /llm <0-{len(agent.list_llms())-1}>', context_token=ctx)
        else:
            lines = [f"{'→' if cur else '  '} [{i}] {name}" for i, name, cur in agent.list_llms()]
            bot.send_text(uid, 'LLMs:\n' + '\n'.join(lines), context_token=ctx)
        return

    def _handle():
        prompt = text if text.startswith('/') else f"If you need to show files to user, use [FILE:filepath] in your response.\n\n{text}"
        dq = agent.put_task(prompt, source="wechat")
        _typing_stop = threading.Event()
        def _keep_typing():
            ticket = bot.get_typing_ticket(uid, ctx)
            if not ticket: return
            while not _typing_stop.is_set():
                try: bot.send_typing(uid, ticket)
                except: pass
                _typing_stop.wait(2.0)
        threading.Thread(target=_keep_typing, daemon=True).start()
        result = ''; sent = 0; mi = 0; last_send = 0; item = {}
        def _wx_send(text):
            s = text.strip(); t0 = time.time()
            try:
                bot.send_text(uid, s, context_token=ctx)
                print(f'[WX] send ok len={len(s)} dt={time.time()-t0:.1f}s', file=sys.__stdout__)
                return True
            except Exception as e:
                print(f'[WX] send err len={len(s)} dt={time.time()-t0:.1f}s {type(e).__name__}: {e}', file=sys.__stdout__)
                return False
        def _send(show):
            nonlocal mi, last_send
            now = time.time()
            if mi >= 9 or not show.strip(): return False
            if mi and now - last_send < 6 * mi: return None
            if _wx_send(show[:3000]): mi += 1; last_send = time.time(); return True
            return False
        try:
            done = []; turn = 1
            while True:
                item = dq.get(timeout=300)
                if 'done' in item: break
                if item.get('turn', turn) > turn:
                    outputs = item.get('outputs', [])
                    lastdone = outputs[-2] if len(outputs) >= 2 else ''
                    turn = item['turn']; done.append(lastdone)
                if len(done) > sent:
                    merged = _clean('\n\n'.join(done[sent:]))
                    print(f'[WX] turns={len(done)}/{len(done)+1} sent={sent} sending={len(done)-sent}', file=sys.__stdout__)
                    if _send(merged): sent = len(done)
        except queue.Empty: result = '[超时]'
        _typing_stop.set()

        if 'done' in item: result, done = item['done'], item.get('outputs', [])
        aborted = _task_aborted.pop(uid, False)
        tag = '[已停止]' if aborted else '[任务已完成]'
        rest = _clean('\n\n'.join(done[sent:] + ['\n\n' + tag]).strip())
        if rest: _wx_send(rest[-3000:])

        files = re.findall(r'\[FILE:([^\]]+)\]', result)
        bad = {'filepath', '<filepath>', 'path', '<path>', 'file_path', '<file_path>', '...'}
        files = [f for f in files if f.strip().lower() not in bad and (f if os.path.isabs(f) else os.path.join(_TEMP_DIR, f)) not in media_paths]
        for fpath in set(files):
            if not os.path.isabs(fpath): fpath = os.path.join(_TEMP_DIR, fpath)
            try:
                if not os.path.exists(fpath): raise FileNotFoundError(f"文件不存在: {fpath}")
                ext = os.path.splitext(fpath)[1].lower()
                sender = bot.send_video if ext in {'.mp4', '.mov', '.m4v', '.webm'} else \
                         bot.send_image if ext in {'.jpg', '.jpeg', '.png', '.gif', '.webp', '.bmp'} else bot.send_file
                sender(uid, fpath, context_token=ctx)
                print(f'[WX] sent media: {fpath}', file=sys.__stdout__)
            except Exception as e: print(f'[WX] send media err: {e}', file=sys.__stdout__)

    threading.Thread(target=_handle, daemon=True).start()

if __name__ == '__main__':
    _do_relogin = '--relogin' in sys.argv
    try: _lock = socket.socket(socket.AF_INET, socket.SOCK_STREAM); _lock.bind(('127.0.0.1', 19531))
    except OSError: print('[WeChat] Another instance running, exiting.'); sys.exit(1)
    _logf = open(os.path.join(os.path.dirname(os.path.dirname(__file__)), 'temp', 'wechatapp.log'), 'a', encoding='utf-8', buffering=1)
    sys.stdout = sys.stderr = _logf
    print(f'[NEW] Process starting {time.strftime("%m-%d %H:%M")}')
    bot = WxBotClient()
    if _do_relogin or not bot.token:
        # QR 登录在无 TTY 的容器里也可用：把二维码打到真实 stdout（docker logs
        # 可见），而不是日志文件——之前在重定向后才判 isatty()，文件句柄恒 false
        # 导致容器内必然退出，无法首次登录。PNG 仍存 ~/.wxbot/wx_qr.png 作兜底。
        sys.stdout = sys.stderr = sys.__stdout__  # restore for QR display (real stdout / container log)
        try:
            bot.login_qr()
        finally:
            sys.stdout = sys.stderr = _logf
    threading.Thread(target=agent.run, daemon=True).start()
    print(f'WeChat Bot 已启动 (bot_id={bot.bot_id})', file=sys.__stdout__)
    try:
        bot.run_loop(on_message)
    except AuthExpired:
        print('[Bot] token expired, exit.', file=sys.__stdout__)
        sys.exit(2)
