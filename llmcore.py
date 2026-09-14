import os, json, re, time, requests, sys, threading, urllib3, base64, importlib, uuid, pathlib, copy
from datetime import datetime
urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)
_RESP_CACHE_KEY = str(uuid.uuid4()); _RESP_CODEX_KEY = str(uuid.uuid4())
_ROOT = os.path.dirname(os.path.abspath(__file__))
if _ROOT not in sys.path: sys.path.append(_ROOT)

_HTTP_POOL_CONNECTIONS = 8
_HTTP_POOL_MAXSIZE = 32

# 2026-08-26 O9: 退避/重试魔法数字命名常量(多版退避逻辑共用口径)。
_BACKOFF_MIN = 0.5          # 指数退避单次最小等待(秒)
_BACKOFF_BASE = 3.0         # _stream_with_retry 指数退避底数
_BACKOFF_CAP = 30.0         # 退避单次上限(秒), 超过则视为不可重试
_BACKOFF_SLEEP_STEP = 0.2   # 可中断退避 sleep 粒度(秒)
_IMG_BACKOFF_BASE = 1.5     # ImageGenClient._delay 指数退避底数(与主链路不同)
_DEFAULT_TRIM_KEEP_RATE = 0.6     # trim 未配置时的默认保留率
_TRIM_KEEP_RATE_DEEPSEEK = 0.3    # deepseek 会话 trim 保留率
_MAXLEN_RATIO = 0.75        # context_win 折算 maxlen_multiplier 系数
_MAXLEN_MULT_FLOOR = 1.0    # maxlen_multiplier 下限
_MAXLEN_MULT_CAP = 3.0      # maxlen_multiplier 上限
_DEFAULT_BASE_DELAY = 3.0   # MixinSession 切换后重试基线延迟(秒)
_MIXIN_RETRY_BASE = 1.5      # MixinSession 退避底数(轮耗尽指数)
_MIXIN_RETRY_CAP = 30.0      # MixinSession 退避上限(秒)

def _build_http_session():
    sess = requests.Session()
    adapter = requests.adapters.HTTPAdapter(
        pool_connections=_HTTP_POOL_CONNECTIONS,
        pool_maxsize=_HTTP_POOL_MAXSIZE,
        max_retries=0,
    )
    sess.mount("http://", adapter)
    sess.mount("https://", adapter)
    return sess

_CONFIG_HTTP = _build_http_session()

def _load_mykeys():
    global _mykey_path
    try:
        sys.modules.pop('mykey', None)
        import mykey; _mykey_path = mykey.__file__
        return {k: v for k, v in vars(mykey).items() if not k.startswith('_')}
    except ImportError as e:
        if getattr(e, 'name', None) != 'mykey':
            raise Exception(f'[ERROR] mykey.py found but failed to import: {e}') from e
    except SyntaxError as e:
        raise Exception(f'[ERROR] mykey.py has syntax error: {e}') from e
    p = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'mykey.json')
    if not os.path.exists(p): raise Exception('[ERROR] mykey.py not found in sys.path and mykey.json not found. Run "python configure_mykey.py" or copy mykey_template.py to mykey.py and fill in your keys.')
    with open(_mykey_path := p, encoding='utf-8') as f: mk = json.load(f)
    if isinstance(mk, dict) and 'remote_url' in mk: return _CONFIG_HTTP.get(mk['remote_url'], timeout=10).json()
    return mk

_mykey_lock = threading.Lock()
_mykey_path = _mykey_mtime = None
def reload_mykeys():
    global _mykey_mtime
    try:
        mt = os.stat(_mykey_path).st_mtime_ns if _mykey_path else -1
        if mt == _mykey_mtime: return globals().get('mykeys', {}), False
        with _mykey_lock: mk = _load_mykeys()
        _mykey_mtime = os.stat(_mykey_path).st_mtime_ns
        print(f'[Info] Load mykeys from {_mykey_path}')
        globals().update(mykeys=mk)
        return mk, True
    except: return globals().get('mykeys', {}), False

def __getattr__(name):  # once guard in PEP 562
    if name == 'mykeys': return reload_mykeys()[0]
    raise AttributeError(f"module 'llmcore' has no attribute {name}")

def compress_history_tags(messages, keep_recent=10, max_len=800, force=False, interval=5):
    """Compress <thinking>/<tool_use>/<tool_result> tags in older messages to save tokens."""
    compress_history_tags._cd = getattr(compress_history_tags, '_cd', 0) + 1
    if force: compress_history_tags._cd = 0
    if compress_history_tags._cd % interval != 0: return messages
    _before = sum(len(json.dumps(m, ensure_ascii=False)) for m in messages)
    _pats = {tag: re.compile(rf'(<{tag}>)([\s\S]*?)(</{tag}>)') for tag in ('thinking', 'think', 'tool_use', 'tool_result')}
    _hist_pat = re.compile(r'<(history|key_info|earlier_context)>[\s\S]*?</\1>')
    def _trunc_str(s): return s[:max_len//2] + '\n...[Truncated]...\n' + s[-max_len//2:] if isinstance(s, str) and len(s) > max_len else s
    def _trunc(text):
        text = _hist_pat.sub(lambda m: f'<{m.group(1)}>[...]</{m.group(1)}>', text)
        for pat in _pats.values(): text = pat.sub(lambda m: m.group(1) + _trunc_str(m.group(2)) + m.group(3), text)
        return text
    for i, msg in enumerate(messages):
        if i >= len(messages) - keep_recent: break
        c = msg['content']
        if isinstance(c, str): msg['content'] = _trunc(c)
        elif isinstance(c, list):
            for b in c:
                if not isinstance(b, dict): continue
                t = b.get('type')
                if t == 'text' and isinstance(b.get('text'), str): b['text'] = _trunc(b['text'])
                elif t == 'thinking' and isinstance(b.get('thinking'), str): b['thinking'] = _trunc_str(b['thinking'])
                elif t == 'tool_result':
                    tc = b.get('content')
                    if isinstance(tc, str): b['content'] = _trunc_str(tc)
                    elif isinstance(tc, list):
                        for sub in tc:
                            if isinstance(sub, dict) and sub.get('type') == 'text': sub['text'] = _trunc_str(sub.get('text'))
                elif t == 'tool_use' and isinstance(b.get('input'), dict):
                    for k, v in b['input'].items(): b['input'][k] = _trunc_str(v)
    print(f"[Cut] {_before} -> {sum(len(json.dumps(m, ensure_ascii=False)) for m in messages)}")
    return messages

def _sanitize_leading_user_msg(msg):
    """把 user 消息里的 tool_result 块改写成纯文本，避免孤立引用。
    history 统一使用 Claude content-block 格式：content 是 list of blocks。"""
    msg = dict(msg)  # 浅拷贝外层 dict
    content = msg.get('content')
    if not isinstance(content, list): return msg
    texts = []
    for block in content:
        if not isinstance(block, dict): continue
        if block.get('type') == 'tool_result':
            c = block.get('content', '')
            if isinstance(c, list): texts.extend(b.get('text', '') for b in c if isinstance(b, dict))
            else: texts.append(str(c))
        elif block.get('type') == 'text': texts.append(block.get('text', ''))
    msg['content'] = [{"type": "text", "text": '\n'.join(t for t in texts if t)}]
    return msg

_oldprint = print
def safeprint(*argv):
    try: _oldprint(*argv)
    except OSError: pass
print = safeprint

STATS = {}

def trim_messages_history(history, sess):
    cap = sess.context_win * 3
    target = int(cap * getattr(sess, 'trim_keep_rate', _DEFAULT_TRIM_KEEP_RATE))
    kp = sess.trim_keep_prefix
    def cost(ms): return sum(len(json.dumps(m, ensure_ascii=False)) for m in ms)
    compress_history_tags(history, interval=getattr(sess, 'cut_msg_interval', 5))
    STATS.update(ctx=(c := cost(history)), msgs=len(history)); print(f'[Debug] Current context: {c} chars, {len(history)} messages.')
    if c <= cap: return
    compress_history_tags(history, keep_recent=4, force=True)
    if cost(history) <= target: return
    pre, post = history[:kp], history[kp:]
    # 0c235a80 的 O(n) 优化曾把 4 个复合操作塞进 1 行, 可读性差。多行化保持
    # 线性复杂度: 成本数组 + 索引推进(pop(0) 整表迁移是 O(n²), 索引截断 O(n))。
    costs = [len(json.dumps(m, ensure_ascii=False)) for m in post]
    c = cost(pre) + sum(costs)
    i = 0
    while len(post) - i > 9 and c > target:
        c -= costs[i]; i += 1
        while i < len(post) and post[i].get('role') != 'user':
            c -= costs[i]; i += 1
        if i < len(post):
            old = costs[i]
            post[i] = _sanitize_leading_user_msg(post[i])
            costs[i] = len(json.dumps(post[i], ensure_ascii=False))
            c += costs[i] - old
    post = post[i:]
    if kp and pre:
        m = pre[-1]
        if m.get('role') == 'assistant' and isinstance(m.get('content'), list):
            m['content'] = [b for b in m['content'] if not (isinstance(b, dict) and b.get('type') == 'tool_use')] or [{"type": "text", "text": "..."}]
        _d = lambda: [{"type": "text", "text": "..."}]
        gap = [{"role": "assistant", "content": _d()}] if m.get('role') == 'user' else [{"role": "user", "content": _d()}, {"role": "assistant", "content": _d()}]
        history[:] = pre + gap + post
    else: history[:] = pre + post
    STATS.update(ctx=(c := cost(history)), msgs=len(history)); print(f'[Debug] Trimmed context, current: {c} chars, {len(history)} messages.')

def auto_make_url(base, path):
    b, p = base.rstrip('/'), path.strip('/')
    if b.endswith('$'): return b[:-1].rstrip('/')
    if b.endswith(p): return b
    return f"{b}/{p}" if re.search(r'/v\d+(/|$)', b) else f"{b}/v1/{p}"

def _parse_claude_json(data):
    if data.get("stop_reason") == "refusal":
        err = "[Error: Claude refusal]"
        yield err
        return [{"type": "text", "text": err}]
    content_blocks = data.get("content", [])
    _record_usage(data.get("usage", {}), "messages")
    for b in content_blocks:
        if b.get("type") == "text": yield b.get("text", "")
        elif b.get("type") == "thinking": yield ""
    return content_blocks

def _raise_if_retryable_overload(emsg):
    """HTTP 200 SSE/body overload → ConnectionError so _stream_with_retry can backoff."""
    if emsg and re.search(r'concurrency|retry later|overloaded|rate.?limit', emsg, re.I):
        raise requests.ConnectionError(emsg)

def _parse_claude_sse(resp_lines):
    """Parse Anthropic SSE stream. Yields text chunks, returns list[content_block]."""
    content_blocks = []; current_block = None; tool_json_buf = ""
    stop_reason = None; got_message_stop = False; warn = None
    for line in resp_lines:
        if not line: continue
        line = line.decode('utf-8') if isinstance(line, bytes) else line
        if not line.startswith("data:"): continue
        data_str = line[5:].lstrip()
        if data_str == "[DONE]": break
        try: evt = json.loads(data_str)
        except Exception as e:
            print(f"[SSE] JSON parse error: {e}, line: {data_str[:200]}")
            continue
        evt_type = evt.get("type", "")
        if evt_type == "message_start":
            usage = evt.get("message", {}).get("usage", {})
            _record_usage(usage, "messages")
        elif evt_type == "content_block_start":
            block = evt.get("content_block", {})
            if block.get("type") == "text": current_block = {"type": "text", "text": ""}
            elif block.get("type") == "thinking": current_block = {"type": "thinking", "thinking": "", "signature": ""}
            elif block.get("type") == "tool_use":
                current_block = {"type": "tool_use", "id": block.get("id", ""), "name": block.get("name", ""), "input": {}}
                tool_json_buf = ""
        elif evt_type == "content_block_delta":
            delta = evt.get("delta", {})
            if delta.get("type") == "text_delta":
                text = delta.get("text", "")
                if current_block and current_block.get("type") == "text": current_block["text"] += text
                if text: yield text
            elif delta.get("type") == "thinking_delta":
                thinking = delta.get("thinking", "")
                if current_block and current_block.get("type") == "thinking": current_block["thinking"] += thinking
                if thinking: yield thinking
            elif delta.get("type") == "signature_delta":
                if current_block and current_block.get("type") == "thinking":
                    current_block["signature"] = current_block.get("signature", "") + delta.get("signature", "")
            elif delta.get("type") == "input_json_delta": tool_json_buf += delta.get("partial_json", "")
        elif evt_type == "content_block_stop":
            if current_block:
                if current_block["type"] == "tool_use":
                    try: current_block["input"] = json.loads(tool_json_buf) if tool_json_buf else {}
                    except: current_block["input"] = {"_raw": tool_json_buf}
                content_blocks.append(current_block)
                current_block = None
        elif evt_type == "message_delta":
            delta = evt.get("delta", {})
            stop_reason = delta.get("stop_reason", stop_reason)
            out_usage = evt.get("usage", {})
            out_tokens = out_usage.get("output_tokens", 0)
            if out_tokens: STATS['out'] = out_tokens; print(f"[Output] tokens={out_tokens} stop_reason={stop_reason}")
        elif evt_type == "message_stop": got_message_stop = True
        elif evt_type == "error":
            err = evt.get("error", {})
            emsg = err.get("message", str(err)) if isinstance(err, dict) else str(err)
            _raise_if_retryable_overload(emsg)  # 走 _stream_with_retry，避免落到 ga 应用层
            warn = f"\n\n!!!Error: SSE {emsg}"; break
    if not warn:
        if not got_message_stop and not stop_reason: warn = "\n\n[!!! 流异常中断，未收到完整响应 !!!]"
        elif stop_reason == "max_tokens": warn = "\n\n[!!! Response truncated: max_tokens !!!]"
        elif stop_reason == "refusal": warn = "\n\n[Error: Claude refusal]"
    if current_block:
        if current_block["type"] == "tool_use":
            try: current_block["input"] = json.loads(tool_json_buf) if tool_json_buf else {}
            except: current_block["input"] = {"_raw": tool_json_buf}
        content_blocks.append(current_block); current_block = None
    if warn:
        print(f"[WARN] {warn.strip()}")
        insert_at = next((i for i,b in enumerate(content_blocks) if b.get("type") == "tool_use"), len(content_blocks))
        content_blocks.insert(insert_at, {"type": "text", "text": warn}); yield warn
    return content_blocks

def _try_parse_tool_args(raw):
    """Parse tool args string; split concatenated JSON objects like {..}{..} if needed.
    Returns list of parsed dicts."""
    if not raw: return [{}]
    try: return [json.loads(raw)]
    except: pass
    parts = re.split(r'(?<=\})(?=\{)', raw)
    if len(parts) > 1:
        parsed = []
        for p in parts:
            try: parsed.append(json.loads(p))
            except: return [{"_raw": raw}]
        return parsed
    return [{"_raw": raw}]

def _parse_openai_sse(resp_lines, api_mode="chat_completions"):
    """Parse OpenAI SSE stream (chat_completions or responses API).
    Yields text chunks, returns list[content_block].
    content_block: {type:'text', text:str} | {type:'tool_use', id:str, name:str, input:dict}
    """
    content_text = ""
    if api_mode == "responses":
        seen_delta = False; fc_buf = {}; current_fc_idx = None; reasoning_text = ""
        for line in resp_lines:
            if not line: continue
            line = line.decode('utf-8', errors='replace') if isinstance(line, bytes) else line
            if not line.startswith("data:"): continue
            data_str = line[5:].lstrip()
            if data_str == "[DONE]": break
            try: evt = json.loads(data_str)
            except: continue
            etype = evt.get("type", "")
            if etype == "response.output_text.delta":
                delta = evt.get("delta", "")
                if delta: seen_delta = True; content_text += delta; yield delta
            elif etype == "response.output_text.done" and not seen_delta:
                text = evt.get("text", "")
                if text: content_text += text; yield text
            elif etype == "response.reasoning_text.delta":
                delta = evt.get("delta", "")
                if delta: reasoning_text += delta
            elif etype == "response.reasoning_text.done":
                text = evt.get("text", "")
                if text: reasoning_text = text
            elif etype == "response.output_item.added":
                item = evt.get("item", {})
                if item.get("type") == "function_call":
                    idx = evt.get("output_index", 0)
                    fc_buf[idx] = {"id": item.get("call_id", item.get("id", "")), "name": item.get("name", ""), "args": ""}
                    current_fc_idx = idx
            elif etype == "response.function_call_arguments.delta":
                idx = evt.get("output_index", current_fc_idx or 0)
                if idx in fc_buf: fc_buf[idx]["args"] += evt.get("delta", "")
            elif etype == "response.function_call_arguments.done":
                idx = evt.get("output_index", current_fc_idx or 0)
                if idx in fc_buf: fc_buf[idx]["args"] = evt.get("arguments", fc_buf[idx]["args"])
            elif etype == "error":
                err = evt.get("error", {})
                emsg = err.get("message", str(err)) if isinstance(err, dict) else str(err)
                _raise_if_retryable_overload(emsg)
                if emsg: content_text += f"!!!Error: {emsg}"; yield f"!!!Error: {emsg}"
                break
            elif etype == "response.completed":
                usage = evt.get("response", {}).get("usage", {})
                _record_usage(usage, api_mode)
                break
            elif etype == "response.incomplete":
                # DeepSeek/OpenAI responses stream may end here (no [DONE]); treat as valid terminal,
                # record usage and surface a truncation marker instead of an empty-response retry storm.
                usage = (evt.get("response") or {}).get("usage", {})
                _record_usage(usage, api_mode)
                reason = ((evt.get("response") or {}).get("incomplete_details") or {}).get("reason", "") or "unknown"
                if not content_text and not fc_buf:
                    marker = f"[!!! output truncated: {reason}]"
                    content_text += marker; yield marker
                break
            elif etype == "response.failed":
                usage = (evt.get("response") or {}).get("usage", {})
                _record_usage(usage, api_mode)
                err = ((evt.get("response") or {}).get("error") or {})
                emsg = err.get("message", str(err)) if isinstance(err, dict) else str(err)
                _raise_if_retryable_overload(emsg)
                if emsg: content_text += f"!!!Error: {emsg}"; yield f"!!!Error: {emsg}"
                break
        blocks = []
        if reasoning_text: blocks.append({"type": "thinking", "thinking": reasoning_text})
        if content_text: blocks.append({"type": "text", "text": content_text})
        for idx in sorted(fc_buf):
            fc = fc_buf[idx]
            inps = _try_parse_tool_args(fc["args"])
            for i, inp in enumerate(inps):
                bid = fc["id"] or ''
                if len(inps) > 1: bid = f"{bid}_{i}" if bid else f"split_{i}"
                blocks.append({"type": "tool_use", "id": bid, "name": fc["name"], "input": inp})
        return blocks
    else:
        tc_buf = {}  # index -> {id, name, args}
        reasoning_text = ""
        for line in resp_lines:
            if not line: continue
            line = line.decode('utf-8', errors='replace') if isinstance(line, bytes) else line
            if not line.startswith("data:"): continue
            data_str = line[5:].lstrip()
            if data_str == "[DONE]": break
            try: evt = json.loads(data_str)
            except: continue
            ch = (evt.get("choices") or [{}])[0]
            delta = ch.get("delta") or {}
            if rc := delta.get("reasoning_content") or delta.get("reasoning", ""):
                reasoning_text += rc; yield rc
            if delta.get("content"):
                text = delta["content"]; content_text += text; yield text
            for tc in (delta.get("tool_calls") or []):
                idx = tc.get("index", 0)
                has_name = bool(tc.get("function", {}).get("name"))
                if idx not in tc_buf:
                    if has_name or not tc_buf: tc_buf[idx] = {"id": tc.get("id") or '', "name": "", "args": ""}
                    else: idx = max(tc_buf)
                if has_name: tc_buf[idx]["name"] = tc["function"]["name"]
                if tc.get("function", {}).get("arguments"): tc_buf[idx]["args"] += tc["function"]["arguments"]
                if tc.get("id") and not tc_buf[idx]["id"]: tc_buf[idx]["id"] = tc["id"]
            usage = evt.get("usage")
            if usage: _record_usage(usage, api_mode)
        blocks = []
        if reasoning_text: blocks.append({"type": "thinking", "thinking": reasoning_text})
        if content_text: blocks.append({"type": "text", "text": content_text})
        for idx in sorted(tc_buf):
            tc = tc_buf[idx]
            inps = _try_parse_tool_args(tc["args"])
            for i, inp in enumerate(inps):
                bid = tc["id"] or ''
                if len(inps) > 1: bid = f"{bid}_{i}" if bid else f"split_{i}"
                blocks.append({"type": "tool_use", "id": bid, "name": tc["name"], "input": inp})
        return blocks

def _record_usage(usage, api_mode):
    if not usage: return
    if api_mode == 'responses':
        cached = (usage.get("input_tokens_details") or {}).get("cached_tokens", 0)
        inp = usage.get("input_tokens", 0); out = usage.get("output_tokens", 0)
        print(f"[Cache] input={inp} cached={cached}")
        if out: print(f"[Output] tokens={out}")
    elif api_mode == 'chat_completions':
        cached = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
        inp = usage.get("prompt_tokens", 0); out = usage.get("completion_tokens", 0)
        print(f"[Cache] input={inp} cached={cached}")
        if out: print(f"[Output] tokens={out}")
    elif api_mode == 'messages':
        ci, cr, raw_inp = usage.get("cache_creation_input_tokens", 0), usage.get("cache_read_input_tokens", 0), usage.get("input_tokens", 0)
        inp, cached, out = raw_inp + ci + cr, cr, 0
        print(f"[Cache] input={raw_inp} creation={ci} read={cr}")
    else: return
    STATS.update(inp=inp, cached=cached, out=out)
    
def _parse_openai_json(data, api_mode="chat_completions"):
    blocks = []
    if api_mode == "responses":
        _record_usage(data.get("usage") or {}, api_mode)
        for item in (data.get("output") or []):
            if item.get("type") == "message":
                for p in (item.get("content") or []):
                    if p.get("type") in ("output_text", "text") and p.get("text"):
                        blocks.append({"type": "text", "text": p["text"]}); yield p["text"]
            elif item.get("type") == "reasoning":
                for p in (item.get("content") or []):
                    if p.get("type") in ("reasoning_text", "summary_text") and p.get("text"):
                        blocks.append({"type": "thinking", "thinking": p["text"]})
            elif item.get("type") == "function_call":
                try: args = json.loads(item.get("arguments", "")) if item.get("arguments") else {}
                except: args = {"_raw": item.get("arguments", "")}
                blocks.append({"type": "tool_use", "id": item.get("call_id", item.get("id", "")),
                               "name": item.get("name", ""), "input": args})
        status = (data.get("status") or "").lower()
        if status == "failed":
            err = data.get("error") or {}
            emsg = err.get("message", str(err)) if isinstance(err, dict) else str(err)
            _raise_if_retryable_overload(emsg)
            if emsg: blocks.append({"type": "text", "text": f"!!!Error: {emsg}"}); yield f"!!!Error: {emsg}"
        elif status == "incomplete" and not any(b.get("type") == "text" for b in blocks):
            reason = ((data.get("incomplete_details") or {}).get("reason", "")) or "unknown"
            marker = f"[!!! output truncated: {reason}]"
            blocks.append({"type": "text", "text": marker}); yield marker
    else:
        _record_usage(data.get("usage") or {}, api_mode)
        msg = (data.get("choices") or [{}])[0].get("message", {})
        reasoning = msg.get("reasoning_content") or msg.get("reasoning", "")
        if reasoning:
            blocks.append({"type": "thinking", "thinking": reasoning})
        content = msg.get("content", "")
        if content:
            blocks.append({"type": "text", "text": content}); yield content
        for tc in (msg.get("tool_calls") or []):
            fn = tc.get("function", {})
            try: args = json.loads(fn.get("arguments", "")) if fn.get("arguments") else {}
            except: args = {"_raw": fn.get("arguments", "")}
            blocks.append({"type": "tool_use", "id": tc.get("id", ""), "name": fn.get("name", ""), "input": args})
    return blocks

def _stamp_oai_cache_markers(messages, model):
    """Add cache_control to last 2 user messages for Anthropic models via OAI-compatible relay."""
    ml = model.lower()
    if not any(k in ml for k in ('claude', 'anthropic')): return
    user_idxs = [i for i, m in enumerate(messages) if m.get('role') == 'user']
    for idx in user_idxs[-2:]:
        c = messages[idx].get('content')
        if isinstance(c, str):
            messages[idx] = {**messages[idx], 'content': [{'type': 'text', 'text': c, 'cache_control': {'type': 'ephemeral'}}]}
        elif isinstance(c, list) and c:
            # cache_control 只落在 text 块(最后块可能是 image_url/image 块,
            # 审查 I-4 连带: 图片任务首轮最后块是图, 标记须落在前面的文本)。
            c = list(c)
            for i in range(len(c) - 1, -1, -1):
                if isinstance(c[i], dict) and c[i].get('type') == 'text':
                    c[i] = dict(c[i], cache_control={'type': 'ephemeral'})
                    break
            messages[idx] = {**messages[idx], 'content': c}

def _stream_with_retry(sess, url, headers, payload, parse_fn):
    STATS['session'] = getattr(sess, 'name', '')
    _RETRYABLE = {408, 409, 425, 429, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526, 527, 529}
    cap = float(getattr(sess, 'max_retry_after', 60.0))
    def _delay(resp, attempt):
        try: ra = float((resp.headers or {}).get("retry-after"))
        except: ra = None
        return None if ra and ra > cap else max(_BACKOFF_MIN, ra or min(_BACKOFF_CAP, _BACKOFF_BASE * (2 ** attempt)))
    def _stopped(): return getattr(sess, 'should_stop', None) and sess.should_stop()
    def _sleep(d):  # interruptible sleep; True if aborted
        end = time.time() + d
        while time.time() < end:
            if _stopped(): return True
            time.sleep(_BACKOFF_SLEEP_STEP)
        return _stopped()
    for attempt in range(sess.max_retries + 1):
        if _stopped(): return []
        streamed = False
        STATS.update(t_start=time.time(), t_ttft=None)
        if not sess.stream: STATS['t_ttft'] = STATS['t_start']
        try:
            with sess.http.post(url, headers=headers, json=payload, stream=sess.stream,
                               timeout=(sess.connect_timeout, sess.read_timeout), proxies=sess.proxies, verify=sess.verify) as r:
                sess.active_response = r
                if r.status_code >= 400:
                    #pathlib.Path(__file__).parent.joinpath('temp','bad_requests.json').write_text(json.dumps({"url":url,"headers":headers,"payload":payload,"t":time.time()},ensure_ascii=False),encoding='utf-8')
                    d = _delay(r, attempt) if r.status_code in _RETRYABLE and attempt < sess.max_retries else None
                    if d is not None:
                        print(f"[LLM Retry] HTTP {r.status_code}, retry in {d:.1f}s ({attempt+1}/{sess.max_retries+1})")
                        if _sleep(d): return []
                        continue
                    try: body = r.text.strip()[:500]
                    except: body = ""
                    err = f"!!!Error: HTTP {r.status_code}" + (f" (retry-after > {cap:.0f}s)" if d is None and r.status_code in _RETRYABLE and attempt < sess.max_retries else "") + (f": {body}" if body else "")
                    yield err; return [{"type": "text", "text": err}]
                gen = parse_fn(r)
                try:
                    while True:
                        if getattr(sess, 'should_stop', None) and sess.should_stop():
                            STATS['t_end'] = time.time(); return []
                        chunk = next(gen)
                        if chunk and STATS.get('t_ttft') is None: STATS['t_ttft'] = time.time()
                        streamed = True; yield chunk
                except StopIteration as e:
                    if not e.value and not streamed: raise requests.ConnectionError("empty response")
                    STATS['t_end'] = time.time()
                    STATS['tps'] = STATS.get('out', 0) / max(1e-9, STATS['t_end'] - max(STATS['t_ttft'] or 0, STATS['t_start']))
                    return e.value or []
        except (requests.Timeout, requests.ConnectionError, requests.exceptions.ChunkedEncodingError) as e:
            err = f"!!!Error: {type(e).__name__}: {e}" if str(e) else f"!!!Error: {type(e).__name__}"
            if getattr(sess, 'should_stop', None) and sess.should_stop(): return []
            if attempt < sess.max_retries:
                d = _delay(None, attempt)
                print(f"[LLM Retry] {type(e).__name__}, retry in {d:.1f}s ({attempt+1}/{sess.max_retries+1})")
                if _sleep(d): return []
                continue
            yield err; return [{"type": "text", "text": err}]
        except Exception as e:
            err = f"\n\n[!!! 流异常中断 {type(e).__name__}: {e} !!!]" if streamed else f"!!!Error: {type(e).__name__}: {e}"
            yield err; return [{"type": "text", "text": err}]

def _openai_stream(sess, messages):
    model, api_mode = sess.model, sess.api_mode
    ml = model.lower()
    temperature = sess.temperature
    if 'kimi' in ml or 'moonshot' in ml: temperature = 1
    elif 'minimax' in ml: temperature = max(0.01, min(temperature, 1.0))  # MiniMax requires temp in (0, 1]
    headers = {"Authorization": f"Bearer {sess.api_key}", "Content-Type": "application/json", "Accept": "text/event-stream", 'originator': 'codex_exec'}
    headers["User-Agent"] = sess.user_agent
    if api_mode == "responses":
        url = auto_make_url(sess.api_base, "responses")
        payload = {"model": model, "input": _to_responses_input(messages), "stream": sess.stream, 
                   "prompt_cache_key": _RESP_CACHE_KEY, "instructions": sess.system or "You are an Omnipotent Executor.",
                   "client_metadata": {"x-codex-window-id": f"{_RESP_CACHE_KEY}:0","x-codex-installation-id": _RESP_CODEX_KEY},
                   'include': ['reasoning.encrypted_content']}
        if sess.reasoning_effort: payload["reasoning"] = {"effort": sess.reasoning_effort}
        if sess.max_tokens: payload["max_output_tokens"] = sess.max_tokens
    else:
        url = auto_make_url(sess.api_base, "chat/completions")
        if sess.system: messages = [{"role": "system", "content": sess.system}] + messages
        _stamp_oai_cache_markers(messages, model)
        payload = {"model": model, "messages": messages, "stream": sess.stream}
        if sess.stream: payload["stream_options"] = {"include_usage": True}
        if temperature != 1: payload["temperature"] = temperature
        if sess.max_tokens: payload["max_completion_tokens" if ml.startswith(("gpt-5", "o1", "o2", "o3", "o4")) else "max_tokens"] = sess.max_tokens
        if sess.reasoning_effort: payload["reasoning_effort"] = sess.reasoning_effort
    tools = getattr(sess, 'tools', None)
    if tools: payload["tools"] = _prepare_oai_tools(tools, api_mode)
    if sess.service_tier: payload["service_tier"] = sess.service_tier
    parse_fn = (lambda r: _parse_openai_sse(r.iter_lines(), api_mode)) if sess.stream else (lambda r: _parse_openai_json(r.json(), api_mode))
    return (yield from _stream_with_retry(sess, url, headers, payload, parse_fn))
        
def _prepare_oai_tools(tools, api_mode="chat_completions"):
    if api_mode == "responses":
        resp_tools = []
        for t in tools:
            if t.get("type") == "function" and "function" in t:
                rt = {"type": "function"}; rt.update(t["function"])
                resp_tools.append(rt)
            else: resp_tools.append(t)
        return resp_tools
    return tools

def _to_responses_input(messages):
    result, pending = [], []
    for msg in messages:
        role = str(msg.get("role", "user")).lower()
        if role == "tool":
            cid = msg.get("tool_call_id") or (pending.pop(0) if pending else f"call_{uuid.uuid4().hex[:8]}")
            result.append({"type": "function_call_output", "call_id": cid, "output": msg.get("content", "")})
            continue
        if role not in ["user", "assistant", "system", "developer"]: role = "user"
        if role == "system": role = "developer"  # Responses API uses 'developer' instead of 'system'
        content = msg.get("content", "")
        text_type = "output_text" if role == "assistant" else "input_text"
        parts = []
        if isinstance(content, str):
            if content: parts.append({"type": text_type, "text": content})
        elif isinstance(content, list):
            for part in content:
                if not isinstance(part, dict): continue
                ptype = part.get("type")
                if ptype == "text":
                    text = part.get("text", "")
                    if text: parts.append({"type": text_type, "text": text})
                elif ptype == "image_url":
                    url = (part.get("image_url") or {}).get("url", "")
                    if url and role != "assistant": parts.append({"type": "input_image", "image_url": url})
        if len(parts) == 0: parts = [{"type": text_type, "text": str(content) if not isinstance(content, list) else '[empty]'}]
        result.append({"role": role, "content": parts})
        pending = []
        for tc in (msg.get("tool_calls") or []):
            f = tc.get("function", {})
            cid = tc.get("id") or f"call_{uuid.uuid4().hex[:8]}"
            pending.append(cid)
            result.append({"type": "function_call", "call_id": cid, "name": f.get("name", ""), "arguments": f.get("arguments", "")})
    return result


def _msgs_claude2oai(messages):
    result = []
    for msg in messages:
        role = msg.get("role", "user")
        content = msg.get("content", "")
        blocks = content if isinstance(content, list) else [{"type": "text", "text": str(content)}]
        if role == "assistant":
            text_parts, tool_calls, reasoning = [], [], ""
            for b in blocks:
                if not isinstance(b, dict): continue
                if b.get("type") == "thinking" and b.get("thinking"): reasoning = b["thinking"]
                elif b.get("type") == "text" and b.get("text"): text_parts.append({"type": "text", "text": b.get("text", "")})
                elif b.get("type") == "tool_use":
                    tool_calls.append({
                        "id": b.get("id") or '', "type": "function",
                        "function": {"name": b.get("name", ""), "arguments": json.dumps(b.get("input", {}), ensure_ascii=False)}
                    })
            m = {"role": "assistant"}
            if reasoning: m["reasoning_content"] = reasoning
            if text_parts: m["content"] = text_parts
            elif not tool_calls: m["content"] = "."
            if tool_calls: m["tool_calls"] = tool_calls
            result.append(m)
        elif role == "user":
            text_parts = []
            for b in blocks:
                if not isinstance(b, dict): continue
                if b.get("type") == "tool_result":
                    if text_parts:
                        result.append({"role": "user", "content": text_parts})
                        text_parts = []
                    tr = b.get("content", "")
                    if isinstance(tr, list):
                        tr = "\n".join(x.get("text", "") for x in tr if isinstance(x, dict) and x.get("type") == "text")
                    result.append({"role": "tool", "tool_call_id": b.get("tool_use_id") or '', "content": tr if isinstance(tr, str) else str(tr)})
                elif b.get("type") == "image":
                    src = b.get("source") or {}
                    if src.get("type") == "base64" and src.get("data"):
                        text_parts.append({"type": "image_url", "image_url": {"url": f"data:{src.get('media_type', 'image/png')};base64,{src.get('data', '')}"}})
                elif b.get("type") == "image_url": text_parts.append(b)
                elif b.get("type") == "text" and b.get("text"): text_parts.append({"type": "text", "text": b.get("text", "")})
            if text_parts: result.append({"role": "user", "content": text_parts})
        else: result.append(msg)
    return result


class BaseSession:
    def __init__(self, cfg):
        self.api_key = cfg['apikey']
        self.api_base = cfg['apibase'].rstrip('/')
        self.model = cfg.get('model', '')
        default_context_win = 35000; default_cut_msg_interval = 7
        if 'deepseek' in self.model.lower():
            default_context_win = 80000; default_cut_msg_interval = 25; self.trim_keep_rate = _TRIM_KEEP_RATE_DEEPSEEK
        self.context_win = cfg.get('context_win', default_context_win)
        self.maxlen_multiplier = min(max(self.context_win / default_context_win * _MAXLEN_RATIO, _MAXLEN_MULT_FLOOR), _MAXLEN_MULT_CAP)
        self.cut_msg_interval = int(default_cut_msg_interval * self.maxlen_multiplier)
        self.trim_keep_prefix = max(0, int(cfg.get('trim_keep_prefix', 0) or 0))
        self.history = []; self.lock = threading.Lock(); self.system = ""
        self.name = cfg.get('name', self.model)
        self.extra_sys_prompt = cfg.get('extra_sys_prompt', '')
        if cfg.get('extra_sys_prompt_file'):
            _epf = cfg['extra_sys_prompt_file'] if os.path.isabs(cfg['extra_sys_prompt_file']) else os.path.join(_ROOT, cfg['extra_sys_prompt_file'])
            with open(_epf, encoding='utf-8') as _f: self.extra_sys_prompt = (self.extra_sys_prompt or '') + _f.read()
        proxy = cfg.get('proxy'); 
        self.proxies = {"http": proxy, "https": proxy} if proxy else None
        self.max_retries = max(0, int(cfg.get('max_retries', 4)))
        self.max_retry_after = float(cfg.get('max_retry_after', 60.0))
        self.verify = cfg.get('verify', True)
        self.stream = cfg.get('stream', True)
        default_ct, default_rt = (5, 40) if self.stream else (10, 240)
        self.connect_timeout = max(1, int(cfg.get('timeout', default_ct)))
        self.read_timeout = max(5, int(cfg.get('read_timeout', default_rt)))
        def _enum(key, valid):
            v = cfg.get(key); v = None if v is None else str(v).strip().lower()
            return v if not v or v in valid else print(f"[WARN] Invalid {key} {v!r}, ignored.")
        self.reasoning_effort = _enum('reasoning_effort', {'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'})
        self.service_tier = _enum('service_tier', {'auto', 'default', 'priority', 'flex'})
        self.thinking_type = _enum('thinking_type', {'adaptive', 'enabled', 'disabled'})
        self.thinking_budget_tokens = cfg.get('thinking_budget_tokens')
        self.omit_thinking = cfg.get('omit_thinking', False)  # Exclude thinking from session history
        mode = str(cfg.get('api_mode', 'chat_completions')).strip().lower().replace('-', '_')
        self.api_mode = 'responses' if mode in ('responses', 'response') else 'chat_completions'
        self.temperature = cfg.get('temperature', 1)
        self.max_tokens = cfg.get('max_tokens')
        self.default_ua = "claude-cli/2.1.152 (external, cli)"
        self.user_agent = cfg.get("user_agent", self.default_ua)
        self.http = _build_http_session()
    def _apply_claude_thinking(self, payload):
        if self.thinking_type:
            thinking = {"type": self.thinking_type}
            if self.thinking_type == 'enabled':
                if self.thinking_budget_tokens is None: print("[WARN] thinking_type='enabled' requires thinking_budget_tokens, ignored.")
                else:
                    thinking["budget_tokens"] = self.thinking_budget_tokens; payload["thinking"] = thinking
            else: payload["thinking"] = thinking
        if self.reasoning_effort:
            effort = {'low': 'low', 'medium': 'medium', 'high': 'high', 'xhigh': 'max', 'max': 'max'}.get(self.reasoning_effort)
            if effort: payload["output_config"] = {"effort": effort}
            else: print(f"[WARN] reasoning_effort {self.reasoning_effort!r} is unsupported for Claude output_config.effort, ignored.")
    def ask(self, prompt):
        def _ask_gen():
            with self.lock:
                self.history.append({"role": "user", "content": [{"type": "text", "text": prompt}]})
                trim_messages_history(self.history, self)
                messages = self.make_messages(self.history)
            content_blocks = None; content = ''
            gen = self.raw_ask(messages)
            try:
                while True: chunk = next(gen); content += chunk; yield chunk
            except StopIteration as e: content_blocks = e.value or []
            if len(content_blocks) > 1: print(f"[DEBUG BaseSession.ask] content_blocks: {content_blocks}")
            for block in (content_blocks or []):
                if block.get('type', '') == 'tool_use':
                    tu = {'name': block.get('name', ''), 'arguments': block.get('input', {})}
                    yield f'<tool_use>{json.dumps(tu, ensure_ascii=False)}</tool_use>'
            if content.strip() and not content.startswith("!!!Error:"): self.history.append({"role": "assistant", "content": [{"type": "text", "text": content}]})
        return _ask_gen()

def _keep_claude_block(b): return not isinstance(b, dict) or b.get("type") != "thinking" or b.get("signature")
def _drop_unsigned_thinking(messages):
    for m in messages:
        c = m.get("content")
        if isinstance(c, list): m["content"] = [b for b in c if _keep_claude_block(b)]
    return messages

def _ensure_thinking_blocks(messages, model):
    """deepseek needs thinking in history!"""
    if 'deepseek' not in model.lower(): return messages
    for m in messages:
        if m.get("role") != "assistant": continue
        c = m.get("content")
        if not isinstance(c, list): continue
        has_thinking = any(isinstance(b, dict) and b.get("type") == "thinking" for b in c)
        if not has_thinking: m["content"] = [{"type": "thinking", "thinking": "...", "signature": "placeholder"}, *c]
    return messages

class ClaudeSession(BaseSession):
    def raw_ask(self, messages):
        messages = _fix_messages(messages)
        if self.max_tokens is None: self.max_tokens = 8192
        headers = {"x-api-key": self.api_key, "Content-Type": "application/json", "anthropic-version": "2023-06-01", "anthropic-beta": "prompt-caching-2024-07-31"}
        payload = {"model": self.model, "messages": messages, "max_tokens": self.max_tokens, "stream": self.stream}
        if self.temperature != 1: payload["temperature"] = self.temperature
        self._apply_claude_thinking(payload)
        if self.system: payload["system"] = [{"type": "text", "text": self.system, "cache_control": {"type": "persistent"}}]
        url = auto_make_url(self.api_base, "messages")
        parse_fn = (lambda r: _parse_claude_sse(r.iter_lines())) if self.stream else (lambda r: _parse_claude_json(r.json()))
        return (yield from _stream_with_retry(self, url, headers, payload, parse_fn))
    def make_messages(self, raw_list):
        msgs = _drop_unsigned_thinking([{"role": m['role'], "content": list(m['content'])} for m in raw_list])
        user_idxs = [i for i, m in enumerate(msgs) if m['role'] == 'user']
        for idx in user_idxs[-2:]:
            content = msgs[idx]["content"]
            # cache_control 只落在 text 块(最后块可能是 image 块, 审查 I-4)。
            for i in range(len(content) - 1, -1, -1):
                block = content[i]
                if isinstance(block, dict) and block.get('type') == 'text':
                    content[i] = dict(block, cache_control={"type": "ephemeral"})
                    break
        return msgs

class LLMSession(BaseSession):
    def raw_ask(self, messages): return (yield from _openai_stream(self, messages))
    def make_messages(self, raw_list): return _msgs_claude2oai(_fix_messages(raw_list))

def _claude_image_block(b):
    """OpenAI image_url 块 → Claude image 块(data URL 语义对等)。

    2026-08-14 审查 I-4: 双轨制语义分裂——NativeClaudeSession/
    ClaudeSession 走 Anthropic 协议, 注入层产出的 image_url 块原样发送
    必然上游 400/丢图(仅 native_oai 通道经 _msgs_claude2oai 透传可用)。
    外部 URL(非 data:)不转换, 透传由上游决定。"""
    if not isinstance(b, dict) or b.get('type') != 'image_url':
        return b
    url = (b.get('image_url') or {}).get('url', '')
    if not isinstance(url, str) or not url.startswith('data:'):
        return b
    try:
        meta, b64 = url[5:].split(',', 1)
        media_type = (meta.split(';')[0] or 'image/png').strip()
    except (ValueError, TypeError):
        return b
    return {"type": "image", "source": {"type": "base64", "media_type": media_type, "data": b64}}


def _fix_messages(messages):
    if not messages: return messages
    W = lambda c: c if isinstance(c, list) else [{"type": "text", "text": str(c)}]
    merged = []
    for m in messages:
        if m.get('role') not in ('user', 'assistant'): continue
        blocks = W(m.get('content', []))
        if merged and m['role'] == merged[-1]['role']:
            merged[-1]['content'] += list(blocks)
        else:
            merged.append({"role": m['role'], "content": list(blocks)})
    while merged and merged[0]['role'] != 'user': merged.pop(0)
    if not merged: return []
    prev_uses = []
    for m in merged:
        c = m['content']
        if m['role'] == 'assistant':
            seen, out = set(), []
            for b in c:
                uid = b.get('id') if isinstance(b, dict) and b.get('type') == 'tool_use' else None
                if uid and uid in seen: continue
                if uid: seen.add(uid)
                out.append(b)
            m['content'] = out
            prev_uses = [b.get('id') for b in out if isinstance(b, dict) and b.get('type') == 'tool_use']
        else:
            got, rest = {}, []
            for b in c:
                tid = b.get('tool_use_id') if isinstance(b, dict) and b.get('type') == 'tool_result' else None
                if tid and tid in prev_uses and tid not in got: got[tid] = b
                elif isinstance(b, dict) and b.get('type') == 'tool_result': rest.append({"type": "text", "text": str(b.get('content', ''))})
                else: rest.append(b)
            m['content'] = [got.get(u) or {"type": "tool_result", "tool_use_id": u, "content": "(error)"} for u in prev_uses] + rest
            prev_uses = []
    for m in merged:
        converted = []
        for b in m['content']:
            # I-4: user 消息的 image_url 块统一转 Claude image 块(Anthropic
            # 协议通道必需); OAI 通道经 _msgs_claude2oai 转回 image_url,
            # 往返一致, 无行为变化。
            if m['role'] == 'user':
                b = _claude_image_block(b)
            if isinstance(b, dict) and b.get('type') == 'text' and not (b.get('text') or '').strip():
                continue
            converted.append(b)
        m['content'] = converted or [{"type": "text", "text": "."}]
    return merged

class NativeClaudeSession(BaseSession):
    native_ua = "claude-cli/2.1.152 (native, cli)"
    def __init__(self, cfg):
        super().__init__(cfg)
        self.fake_cc_system_prompt = cfg.get("fake_cc_system_prompt", False)
        self._session_id = str(uuid.uuid4())
        self._account_uuid = str(uuid.uuid4())
        self._device_id = uuid.uuid4().hex + uuid.uuid4().hex[:32]
        self.tools = None
        if self.user_agent == self.default_ua: self.user_agent = self.native_ua
        self.api_key_header = str(cfg.get('api_key_header', 'auto')).strip().lower()
    def raw_ask(self, messages):
        if self.max_tokens is None: self.max_tokens = 8192
        model = self.model
        messages = _fix_messages(messages)
        if 'claude' in model.lower(): messages = _drop_unsigned_thinking(messages)
        messages = _ensure_thinking_blocks(messages, self.model)
        beta_parts = ["claude-code-20250219", "interleaved-thinking-2025-05-14", "redact-thinking-2026-02-12", "thinking-token-count-2026-05-13", "context-management-2025-06-27", "prompt-caching-scope-2026-01-05", "mid-conversation-system-2026-04-07", "effort-2025-11-24", "fallback-credit-2026-06-01"]
        if "[1m]" in model.lower():
            beta_parts.insert(1, "context-1m-2025-08-07"); model = model.replace("[1m]", "").replace("[1M]", "")
        headers = {"Content-Type": "application/json", "anthropic-version": "2023-06-01",
            "anthropic-beta": ",".join(beta_parts), "anthropic-dangerous-direct-browser-access": "true",
            "user-agent": self.user_agent, "x-app": "cli"}
        headers.update({"Accept": "application/json", "X-Claude-Code-Session-Id": self._session_id, "X-Stainless-Arch": "x64", "X-Stainless-Lang": "js", "X-Stainless-OS": "Windows", "X-Stainless-Package-Version": "0.94.0", "X-Stainless-Retry-Count": "0", "X-Stainless-Runtime": "node", "X-Stainless-Runtime-Version": "v24.3.0", "X-Stainless-Timeout": "600"})
        if self.api_key_header == 'x-api-key': headers["x-api-key"] = self.api_key
        elif self.api_key_header == 'bearer': headers["authorization"] = f"Bearer {self.api_key}"
        elif self.api_key.startswith("sk-ant-"): headers["x-api-key"] = self.api_key
        else: headers["authorization"] = f"Bearer {self.api_key}"
        payload = {"model": model, "messages": messages, "max_tokens": self.max_tokens, "stream": self.stream}
        #if self.fake_cc_system_prompt: payload["max_tokens"] = 64000
        if self.temperature != 1: payload["temperature"] = self.temperature
        self._apply_claude_thinking(payload)
        #payload["context_management"] = {"edits": [{"type": "clear_thinking_20251015", "keep": "all"}]}; 
        if self.fake_cc_system_prompt:
            if 'thinking' not in payload: payload["thinking"] = {"type": "adaptive"}
            if 'output_config' not in payload: payload["output_config"] = {"effort": "medium"}
        payload["metadata"] = {"user_id": json.dumps({"device_id": self._device_id, "account_uuid": "", "session_id": self._session_id}, separators=(',', ':'))}
        if self.tools:
            claude_tools = openai_tools_to_claude(self.tools)
            tools = [dict(t) for t in claude_tools]; tools[-1]["cache_control"] = {"type": "ephemeral"}
            payload["tools"] = tools
        else: print("[ERROR] No tools provided for this session.")
        payload['system'] = [{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude.", "cache_control": {"type": "ephemeral"}}]
        #payload['system'][0]['text'] += f"\nPlatform: {sys.platform}"
        if self.system:
            if self.fake_cc_system_prompt: payload["system"].append({"type": "text", "text": self.system})
            else: payload["system"] = [{"type": "text", "text": self.system}]
        user_idxs = [i for i, m in enumerate(messages) if m['role'] == 'user']
        for idx in user_idxs[-2:]:
            messages[idx] = {**messages[idx], "content": list(messages[idx]["content"])}
            # cache_control 只落在 text 块(最后块可能是 image 块, 审查 I-4)。
            content = messages[idx]["content"]
            for i in range(len(content) - 1, -1, -1):
                block = content[i]
                if isinstance(block, dict) and block.get('type') == 'text':
                    content[i] = dict(block, cache_control={"type": "ephemeral"})
                    break
        url = auto_make_url(self.api_base, "messages") + '?beta=true'
        parse_fn = (lambda r: _parse_claude_sse(r.iter_lines())) if self.stream else (lambda r: _parse_claude_json(r.json()))
        return (yield from _stream_with_retry(self, url, headers, payload, parse_fn))

    def ask(self, msg):
        assert type(msg) is dict
        with self.lock:
            self.history.append(msg)
            trim_messages_history(self.history, self)
            messages = [{"role": m["role"], "content": list(m["content"])} for m in self.history]
        content_blocks = None
        gen = self.raw_ask(messages)
        try:
            while True: yield next(gen)
        except StopIteration as e: content_blocks = e.value or []
        if content_blocks and (_injected := _ensure_text_block(content_blocks)): yield _injected
        if content_blocks and not (len(content_blocks) == 1 and content_blocks[0].get("text", "").startswith("!!!Error:")):
            history_blocks = content_blocks
            if self.omit_thinking: history_blocks = [b for b in content_blocks if b.get("type") != "thinking"]
            self.history.append({"role": "assistant", "content": history_blocks})
        text_parts = [b["text"] for b in content_blocks if b.get("type") == "text"]
        content = "\n".join(text_parts).strip()
        tool_calls = [MockToolCall(b["name"], b.get("input", {}), id=b.get("id", "")) for b in content_blocks if b.get("type") == "tool_use"]
        if not tool_calls: tool_calls, content = _parse_text_tool_calls(content)
        thinking_parts = [b["thinking"] for b in content_blocks if b.get("type") == "thinking"]
        thinking = "\n".join(thinking_parts).strip()
        if not thinking:
            think_pattern = r"<think(?:ing)?>(.*?)</think(?:ing)?>"
            think_match = re.search(think_pattern, content, re.DOTALL)
            if think_match:
                thinking = think_match.group(1).strip()
                content = re.sub(think_pattern, "", content, flags=re.DOTALL)
        raw = "[" + ",\n".join(repr(b) for b in content_blocks) + "]"
        return MockResponse(thinking, content, tool_calls, raw)

class NativeOAISession(NativeClaudeSession):
    native_ua = "codex_exec/0.139.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.139.0)"
    def raw_ask(self, messages):
        messages = _fix_messages(messages)
        messages = _ensure_thinking_blocks(messages, self.model)
        return (yield from _openai_stream(self, _msgs_claude2oai(messages)))

def openai_tools_to_claude(tools):
    """[{type:'function', function:{name,description,parameters}}] → [{name,description,input_schema}]."""
    result = []
    for t in tools:
        if 'input_schema' in t: result.append(t); continue  # 已是claude格式
        fn = t.get('function', t)
        result.append({'name': fn['name'], 'description': fn.get('description', ''),
            'input_schema': fn.get('parameters', {'type': 'object', 'properties': {}})})
    return result

class MockFunction:
    def __init__(self, name, arguments): self.name, self.arguments = name, arguments  
         
class MockToolCall:
    def __init__(self, name, args, id=''):
        arg_str = json.dumps(args, ensure_ascii=False) if isinstance(args, (dict, list)) else (args or '{}')
        self.function = MockFunction(name, arg_str); self.id = id

class MockResponse:
    def __init__(self, thinking, content, tool_calls, raw, stop_reason='end_turn'):
        self.thinking = thinking; self.content = content          
        self.tool_calls = tool_calls; self.raw = raw
        self.stop_reason = 'tool_use' if tool_calls else stop_reason
    def __repr__(self):    
        return f"<MockResponse thinking={bool(self.thinking)}, content='{self.content}', tools={bool(self.tool_calls)}>"

def _flatten_prompt_content(content):
    """协议通道(ToolClient)拍平 content 为文本: image/image_url 块降级为
    占位字符串。2026-08-14 审查 S-1: 旧实现 str(list) 把整段 base64 文本
    垃圾注入提示词/历史/日志(每轮重发最多 ~3.5MB, 模型却看不到图)。保持
    list repr 形态, 仅图片块替换, 文本块格式零变化。"""
    if not isinstance(content, list):
        return str(content)
    parts = []
    for b in content:
        if isinstance(b, dict) and b.get('type') in ('image', 'image_url'):
            parts.append('{"type": "image", "note": "[image omitted: protocol channel has no vision]"}')
        else:
            parts.append(str(b))
    return str(parts)


class ToolClient:
    def __init__(self, backend, auto_save_tokens=True):
        self.backend = backend
        self.auto_save_tokens = auto_save_tokens
        self.last_tools = ''
        self.name = self.backend.name
        self.total_cd_tokens = 0
        self.log_path = None

    def chat(self, messages, tools=None):
        tools = json.loads(json.dumps(tools, ensure_ascii=False)) if tools else tools
        for t in tools or []:
            f = t.get('function', {})
            if f.get('name') == 'file_write':
                props = f.get('parameters', {}).get('properties', {})
                props.pop('content', None)
                extra = '. Content must be placed in <file_content> tags in reply body, not in args'
                if extra not in f.get('description', ''): f['description'] = f.get('description', '') + extra
                break
        full_prompt = self._build_protocol_prompt(messages, tools)
        print("Full prompt length:", len(full_prompt), 'chars')
        gen = self.backend.ask(full_prompt)
        _write_llm_log('Prompt', full_prompt, self.log_path)
        raw_text = ''
        for chunk in gen:
            raw_text += chunk; yield chunk
        _write_llm_log('Response', raw_text, self.log_path, model=self.backend.model)
        return self._parse_mixed_response(raw_text)

    def _prepare_tool_instruction(self, tools):
        tool_instruction = ""
        if not tools: return tool_instruction
        tools_json = json.dumps(tools, ensure_ascii=False, separators=(',', ':'))
        _en = os.environ.get('GA_LANG') == 'en'
        if _en:
            tool_instruction = f"""
### Interaction Protocol (must follow strictly, always in effect)
Follow these steps to think and act:
1. **Think**: Analyze the current situation and strategy inside `<thinking>` tags.
2. **Summarize**: Output a minimal one-line (<30 words) physical snapshot in `<summary>`: new info from last tool result + current tool call intent. This goes into long-term working memory. Must contain real information, no filler.
3. **Act**: If you need to call tools, output one or more **<tool_use> blocks** after your reply, then stop.
"""
        else:
            tool_instruction = f"""
### 交互协议 (必须严格遵守，持续有效)
请按照以下步骤思考并行动：
1. **思考**: 在 `<thinking>` 标签中先进行思考，分析现状和策略。
2. **总结**: 在 `<summary>` 中输出*极为简短*的高度概括的单行（<30字）物理快照，包括上次工具调用结果产生的新信息+本次工具调用意图。此内容将进入长期工作记忆，记录关键信息，严禁输出无实际信息增量的描述。
3. **行动**: 如需调用工具，请在回复正文之后输出一个（或多个）**<tool_use>块**，然后结束。
"""
        tool_instruction += f'\nFormat: ```<tool_use>{{"name": "tool_name", "arguments": {{...}}}}</tool_use>```\n\n### Tools (mounted, always in effect):\n{tools_json}\n'
        if self.auto_save_tokens and self.last_tools == tools_json:
            tool_instruction = "\n### Tools: still active, **ready to call**. Protocol unchanged.\n" if _en else "\n### 工具库状态：持续有效（code_run/file_read等），**可正常调用**。调用协议沿用。\n"
        else: self.total_cd_tokens = 0
        self.last_tools = tools_json
        return tool_instruction

    def _build_protocol_prompt(self, messages, tools):
        system_content = next((m['content'] for m in messages if m['role'].lower() == 'system'), "")
        history_msgs = [m for m in messages if m['role'].lower() != 'system']
        tool_instruction = self._prepare_tool_instruction(tools)
        system = ""; user = ""
        if system_content: system += f"{system_content}\n"
        system += f"{tool_instruction}"
        for m in history_msgs:
            role = "USER" if m['role'] == 'user' else "ASSISTANT"
            user += f"=== {role} ===\n"
            for tr in m.get('tool_results', []): user += f'<tool_result>{tr["content"]}</tool_result>\n'
            # S-1: 协议通道无视觉, image 块降级为占位(不再 str() 输出 base64)。
            flat = _flatten_prompt_content(m['content'])
            user += flat + "\n"
            # Round16-G2: 只累计本条消息增量(原实现把累积拼接的整个 user
            # 字符串重复相加, O(n²) 虚高——10 条 1000 字符消息即越过 9000
            # 阈值, 导致 last_tools 频繁重置、工具描述每轮重新注入)。
            self.total_cd_tokens += len(flat) // 3
        if self.total_cd_tokens > 9000: self.last_tools = ''
        user += "=== ASSISTANT ===\n" 
        return system + user

    def _parse_mixed_response(self, text):
        remaining_text = text; thinking = ''
        think_match = re.search(r"<think(?:ing)?>(.*?)</think(?:ing)?>", text, re.DOTALL)
        if think_match:
            thinking = think_match.group(1).strip()
            remaining_text = re.sub(r"<think(?:ing)?>(.*?)</think(?:ing)?>", "", remaining_text, flags=re.DOTALL)
        tool_calls, remaining_text = _parse_text_tool_calls(remaining_text)
        if not tool_calls:
            json_strs = []; errors = []
            if '<tool_use>' in remaining_text:
                weaktoolstr = remaining_text.split('<tool_use>')[-1].strip().strip('><')
                json_str = weaktoolstr if weaktoolstr.endswith('}') else ''
                if json_str == '' and '```' in weaktoolstr and weaktoolstr.split('```')[0].strip().endswith('}'):
                    json_str = weaktoolstr.split('```')[0].strip()
                if json_str: json_strs.append(json_str)
                remaining_text = remaining_text.replace('<tool_use>'+weaktoolstr, "")
            elif '"name":' in remaining_text and '"arguments":' in remaining_text:
                json_match = re.search(r'\{.*"name":.*\}', remaining_text, re.DOTALL)
                if json_match:
                    json_strs.append(json_match.group(0).strip())
                    remaining_text = remaining_text.replace(json_match.group(0), "").strip()
            for json_str in json_strs:
                try:
                    data = tryparse(json_str)
                    func_name = data.get('name') or data.get('function') or data.get('tool')
                    args = data.get('arguments') or data.get('args') or data.get('params') or data.get('parameters')
                    if args is None: args = data
                    if func_name: tool_calls.append(MockToolCall(func_name, args))
                except json.JSONDecodeError:
                    errors.append(f'Failed to parse tool_use JSON: {json_str[:200]}')
                    self.last_tools = ''
                except: pass
            if not tool_calls:
                for e in errors:
                    print(f"[Warn] {e}"); tool_calls.append(MockToolCall('bad_json', {'msg': e}))
        return MockResponse(thinking, remaining_text.strip(), tool_calls, text)

def _parse_text_tool_calls(content):
    """Fallback: extract tool calls from text when model doesn't use native tool_use blocks."""
    tcs = []
    # try JSON array: [{"type":"tool_use", "name":..., "input":...}]
    _jp = next((p for p in ['[{"type":"tool_use"', '[{"type": "tool_use"'] if p in content), None)
    if _jp and content.endswith('}]'):
        try:
            idx = content.index(_jp); raw = json.loads(content[idx:])
            tcs = [MockToolCall(b["name"], b.get("input", {}), id=b.get("id", "")) for b in raw if b.get("type") == "tool_use"]
            return tcs, content[:idx].strip()
        except: pass
    # try XML tags: <tool_call>{"name":..., "arguments":...}</tool_call>
    _xp = r"<(?:tool_use|tool_call)>((?:(?!<(?:tool_use|tool_call)>).){15,}?)</(?:tool_use|tool_call)>"
    for s in re.findall(_xp, content, re.DOTALL):
        try:
            d = tryparse(s.strip()); name = d.get('name')
            args = d.get('arguments') or d.get('args') or d.get('input') or {}
            if name: tcs.append(MockToolCall(name, args))
        except: pass
    if tcs: content = re.sub(_xp, "", content, flags=re.DOTALL).strip()
    return tcs, content

def _ensure_text_block(blocks):
    """If response has thinking but no text block, inject a synthetic summary from thinking's first line."""
    if any(b.get("type") == "text" for b in blocks): return None
    th = next((b.get("thinking", "") for b in blocks if b.get("type") == "thinking"), "")
    if not th: return None
    line = th.strip().split('\n', 1)[0]
    txt = "<summary>" + (line[:60] + '...' if len(line) > 60 else line) + "</summary>"
    blocks.insert(1, {"type": "text", "text": txt})
    return txt

def _write_llm_log(label, content, log_path=None, model=''):
    if log_path is False: return
    if not log_path:
        log_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), f'temp/model_responses/model_responses_{os.getpid()}.txt')
    os.makedirs(os.path.dirname(os.path.abspath(log_path)), exist_ok=True)
    ts = datetime.now().strftime('%Y-%m-%d %H:%M:%S')
    if model: model = f' model={model}'
    with open(log_path, 'a', encoding='utf-8', errors='replace') as f:
        f.write(f"=== {label} === {ts}{model}\n{content}\n\n")

def tryparse(json_str):
    try: return json.loads(json_str)
    except: pass
    json_str = json_str.strip().strip('`').replace('json\n', '', 1).strip()
    try: return json.loads(json_str)
    except: pass
    try: return json.loads(json_str[:-1])
    except: pass
    if '}' in json_str: json_str = json_str[:json_str.rfind('}') + 1]
    return json.loads(json_str)

def _has_visible_output(blocks):
    """判定一次 LLM 调用的产出是否有用户可见内容(供 MixinSession 空结果切换)。

    text 块(含 summary-only, 语义由上层判定)与 tool_use 块都算产出;
    thinking 块是思考过程, 不算。空列表/仅 thinking = 退化响应。
    """
    for b in blocks or []:
        if not isinstance(b, dict):
            return True
        if b.get('type') == 'tool_use':
            return True
        if b.get('type') == 'text':
            t = b.get('text', '')
            if isinstance(t, str) and t.strip():
                return True
    return False


class MixinSession:
    """A Session facade backed by multiple routed transport sessions."""
    _TRANSPORT_OVERRIDES = frozenset({
        'stream', 'connect_timeout', 'read_timeout', 'temperature', 'max_tokens',
        'reasoning_effort', 'service_tier', 'thinking_type',
        'thinking_budget_tokens', 'omit_thinking', 'proxies', 'verify',
    })
    _FACADE_STATE = frozenset({'name', 'history', 'system', 'tools', 'lock'})

    def __init__(self, all_sessions, cfg):
        self._retries = cfg.get('max_retries', 3)
        self._base_delay = cfg.get('base_delay', _DEFAULT_BASE_DELAY)
        self._spring_sec = cfg.get('spring_back', 300)
        selected = [all_sessions[i].backend if isinstance(i, int) else
                    next(s.backend for s in all_sessions if type(s) is not dict and s.backend.name == i)
                    for i in cfg.get('llm_nos', [])]
        if not selected: raise ValueError('MixinSession: no sessions selected')
        native_groups = {isinstance(s, NativeClaudeSession) for s in selected}
        if len(native_groups) != 1:
            raise ValueError(f"MixinSession: sessions must be in same group (Native or non-Native), got {[type(s).__name__ for s in selected]}")

        self._sessions = [copy.copy(s) for s in selected]
        for s in self._sessions: s.max_retries = 0
        self._native = native_groups.pop()
        self._ask_impl = selected[0].ask.__func__
        self._cur_idx, self._switched_at = 0, 0.0

        primary = self._sessions[0]
        self.name = '|'.join(s.name for s in self._sessions)
        self.history = copy.deepcopy(primary.history)
        self.system = primary.system
        self.tools = getattr(primary, 'tools', None)
        self.lock = threading.Lock()
    @property
    def primary(self): return self._sessions[0]
    @property
    def current(self): return self._sessions[self._cur_idx]
    @property
    def current_name(self): return self.current.name
    def __getattr__(self, name): return getattr(self.current, name)
    def __setattr__(self, name, value):
        sessions = self.__dict__.get('_sessions')
        if sessions and name in self._TRANSPORT_OVERRIDES:
            for s in sessions: setattr(s, name, value)
            return
        node_owns = sessions and any(
            name in s.__dict__ or any(name in cls.__dict__ for cls in type(s).__mro__)
            for s in sessions)
        if node_owns and name not in self._FACADE_STATE: raise AttributeError(f"MixinSession.{name} is node-specific and read-only")
        object.__setattr__(self, name, value)
    def ask(self, prompt):
        self._pick()  # Select the node before ask() reads its context limits.
        return self._ask_impl(self, prompt)
    def make_messages(self, messages): return messages
    def _pick(self):
        if self._cur_idx and time.time() - self._switched_at > self._spring_sec: self._cur_idx = 0
        return self._cur_idx
    def _prepare(self, idx, messages):
        session = self._sessions[idx]
        session.system = self.system
        session.tools = openai_tools_to_claude(self.tools) if self.tools and type(session) is NativeClaudeSession else self.tools
        return messages if self._native else session.make_messages(messages)
    def raw_ask(self, messages):
        base, n = self._pick(), len(self._sessions)
        test_error = lambda x: isinstance(x, str) and x.lstrip().startswith(('!!!Error:', '[Error:'))
        for attempt in range(self._retries + 1):
            idx = (base + attempt) % n
            session = self._sessions[idx]
            gen = session.raw_ask(self._prepare(idx, messages))
            print(f'[MixinSession] Using session ({session.name})')
            last_chunk, return_val, yielded = None, [], False
            try:
                while True:
                    chunk = next(gen); last_chunk = chunk
                    if not yielded and test_error(chunk): continue
                    yield chunk; yielded = True
            except StopIteration as e: return_val = e.value or []
            is_err = test_error(last_chunk)
            # 空结果防护(2026-08-12 生产实证): 上游退化响应(仅 thinking/
            # 空白 content, 无 text 无 tool_use)不是 !!!Error, 但同样需要切
            # 换 session——否则空结果会被 GA 当作正常完成, 用户收到"任务完成"
            # 却没有实际回答。summary-only 文本块仍是 text(本层放行, 由
            # agent 的 do_no_tool 可见文本检查兜底)。
            empty_out = not is_err and not _has_visible_output(return_val)
            if not is_err and not empty_out:
                if attempt > 0: self._cur_idx = idx; self._switched_at = time.time()
                elif isinstance(last_chunk, str) and '[!!! 流异常中断' in last_chunk and n > 1:
                    self._cur_idx = (idx + 1) % n; self._switched_at = time.time()
                    print(f'[MixinSession] Partial failure, next call → s{self._cur_idx} ({self.current.name})')
                return return_val
            if attempt >= self._retries:
                # 所有 session 均已重试: 错误场景保留错误文本(调用方展示),
                # 空场景返回空结果(agent 空白重试逻辑兜底, 连续 3 次 LLM_FAILED)。
                if is_err: yield last_chunk
                return return_val
            nxt = (base + attempt + 1) % n
            if nxt == base:
                rnd = (attempt + 1) // n
                delay = min(_MIXIN_RETRY_CAP, self._base_delay * (_MIXIN_RETRY_BASE ** rnd))
                print(f'[MixinSession] {last_chunk[:80]}, round {rnd} exhausted, retry in {delay:.1f}s')
                time.sleep(delay)
            else:
                reason = f'empty output from s{idx} ({session.name})' if empty_out else f'{last_chunk[:80]}'
                print(f'[MixinSession] {reason}, retry {attempt+1}/{self._retries} (s{idx}→s{nxt})')

THINKING_PROMPT_ZH = """
### 行动规范（持续有效）
每次回复（含工具调用轮）都先在回复文字中包含一个<summary></summary> 中输出极简单行（<30字）物理快照：上次结果新信息+本次意图。此内容进入长期工作记忆。
\n**若用户需求未完成，必须进行工具调用！**
""".strip()
THINKING_PROMPT_EN = """
### Action Protocol (always in effect)
The reply body should first include a minimal one-line (<30 words) physical snapshot in <summary></summary>: new info from last result + current intent. This goes into long-term working memory.
\n**If the user's request is not yet complete, tool calls are required!**
""".strip()

class NativeToolClient:
    @staticmethod
    def _thinking_prompt(): return THINKING_PROMPT_EN if os.environ.get('GA_LANG') == 'en' else THINKING_PROMPT_ZH
    def __init__(self, backend):
        self.backend = backend
        self.backend.system = self._thinking_prompt()
        self.name = self.backend.name
        self._pending_tool_ids = []
        self.log_path = None
    def set_system(self, extra_system):
        combined = f"{extra_system}\n\n{self._thinking_prompt()}" if extra_system else self._thinking_prompt()
        if combined != self.backend.system: print(f"[Debug] Updated system prompt, length {len(combined)} chars.")
        self.backend.system = combined
    def chat(self, messages, tools=None):
        if tools: self.backend.tools = tools
        if not self.backend.history: self._pending_tool_ids = []
        combined_content = []; resp = None; tool_results = []
        for msg in messages:
            c = msg.get('content', '')
            if msg['role'] == 'system': 
                self.set_system(c); continue
            if isinstance(c, str): combined_content.append({"type": "text", "text": c})
            elif isinstance(c, list): combined_content.extend(c)
            if msg['role'] == 'user' and msg.get('tool_results'): tool_results.extend(msg['tool_results'])
        tr_id_set = set();  tool_result_blocks = []
        for tr in tool_results:
            tool_use_id, content = tr.get("tool_use_id", ""), tr.get("content", "")
            tr_id_set.add(tool_use_id)
            if tool_use_id: tool_result_blocks.append({"type": "tool_result", "tool_use_id": tool_use_id, "content": tr.get("content", "")})
            else: combined_content = [{"type": "text", "text": f'<tool_result>{content}</tool_result>'}] + combined_content
        for tid in self._pending_tool_ids:
            if tid not in tr_id_set: tool_result_blocks.append({"type": "tool_result", "tool_use_id": tid, "content": ""})
        self._pending_tool_ids = []
        # Filter whitespace-only text blocks that cause 400 on strict API proxies.
        # 2026-08-14 架构修复(生产实证: 图片任务模型永远看不到图): 原过滤
        # 用 c.get("text", "") 判断——image_url/image 块没有 text 字段, 被判
        # 为空白块整个丢弃, 多模态注入被静默吞掉(GA 日志有 injected, 模型
        # 首轮却没有图)。必须显式保留非 text 块。
        filtered_content = [
            c for c in combined_content
            if c.get("type") in ("image_url", "image") or c.get("text", "").strip()
        ]
        final_content = tool_result_blocks + filtered_content
        if not final_content: final_content = [{"type": "text", "text": "."}]
        merged = {"role": "user", "content": final_content}
        prompt_raw = '{"role": "user", "content": [\n' + ",\n".join(json.dumps(b, ensure_ascii=False) for b in final_content) + "]}"
        _write_llm_log('Prompt', prompt_raw, self.log_path)
        gen = self.backend.ask(merged)
        try:
            while True: 
                chunk = next(gen); yield chunk
        except StopIteration as e: resp = e.value
        if resp: _write_llm_log('Response', resp.raw, self.log_path, model=self.backend.model)
        if resp and hasattr(resp, 'tool_calls') and resp.tool_calls: self._pending_tool_ids = [tc.id for tc in resp.tool_calls]
        return resp

def resolve_session(cfg_name):
    cfg = reload_mykeys()[0].get(cfg_name)
    if not cfg: raise ValueError(f"Config '{cfg_name}' not in mykey")
    cfg['_mykey_name'] = cfg_name
    if 'native' in cfg_name: return (NativeClaudeSession if 'claude' in cfg_name else NativeOAISession)(cfg=cfg)
    if 'claude' in cfg_name: return ClaudeSession(cfg=cfg)
    return LLMSession(cfg=cfg) if 'oai' in cfg_name else None

def resolve_client(cfg_name):
    s = resolve_session(cfg_name)
    return (NativeToolClient(s) if isinstance(s, (NativeClaudeSession, NativeOAISession)) else ToolClient(s)) if s else None

def fast_ask(prompt, cfg_name):
    sess = resolve_session(cfg_name)
    if not sess: raise ValueError(f"fast_ask: '{cfg_name}' unsupported")
    return "".join(sess.raw_ask([{"role": "user", "content": prompt}]))


# ═══════════════════════════════════════════════════════════════════════════
# 生图能力（Phase B image_gen，2026-08-14 定稿）
#
# 设计真值: .tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md §3-§8
# 双形态设计: 直连/托管 = 配置差异(apibase/apikey 指向不同端点), 一份代码
# (对齐 BaseSession 先例: chat 直连与平台托管共用一份实现)。两种形态均已
# 实施(2026-08-14): 直连 = 真实上游/中转网关 + 真实密钥; 托管 = llm-proxy +
# llm.image 能力令牌(平台 runtime_config 下发 image_gen 块, GA 侧零改动)。
#
# 实现进 llmcore.py, 严禁新增 imagegen.py —— 沙箱 overlay 只物化固定清单
# (runtime_overlay.py LEGACY_MODULES), 新增模块导致平台沙箱启动
# ImportError 全平台任务失败(二轮审查 I-1)。
# ═══════════════════════════════════════════════════════════════════════════

# 重试退避集合: 仿 _stream_with_retry(447-487)语义, 生图返回体是 JSON 非文本流
# 不直接复用该函数。
_IMAGE_GEN_RETRYABLE = {408, 409, 425, 429, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526, 527, 529}
# Go 交付上限 image ≤20MiB (delivery_capture.go fail-closed): 客户端 url 直下
# 兜底也在此限流; ga.py 落盘前同值检查(从本模块导入, 单一真值)。
_IMAGE_GEN_MAX_BYTES = 20 * 1024 * 1024

# ── 参数协商自愈(2026-09-13 真实上游实测新增) ──────────────────────────
# "OpenAI 兼容" ≠ "参数全集通用": 上游按"队列"裁剪参数。实测 new-api 中转 →
# agnes-image-2.5-flash 的 text image queue 只收 model/prompt/size/ratio/extra_body,
# 收到 output_format / quality 一律 400 invalid_request("... is not supported by
# text image queue"); 而 gpt-image-2 恰好接受 output_format。因此客户端不能把
# OpenAI 参数全集无条件下发, 也不能按模型名黑名单(方案 §4 原则不在客户端点名模型)
# ——改为"错误文本驱动的参数协商": 400/422 且错误文本点名了本次请求里的可裁剪
# 参数时, 裁剪该参数后重试(独立预算, 不消耗 max_retries)。
_IMAGE_GEN_PARAM_TRIM_STATUS = {400, 422}
_IMAGE_GEN_MAX_PARAM_TRIMS = 3
# 可裁剪参数 = 缺席时上游会给出合理默认、且不改变用户可见契约的可选参数。
# 刻意不含 size(计费必传/模型必需)、n(张数属用户契约, 静默缩水=不诚实)、
# model/prompt(必需)——这几类失败应如实回给模型, 由错误文本引导其改参重试。
_IMAGE_GEN_TRIMMABLE = ("output_format", "quality", "response_format", "stream", "partial_images")
# 错误文本里"参数不被支持"的话术族(命中才进入裁剪判定, 避免误裁无关 400)。
_IMAGE_GEN_PARAM_UNSUPPORTED_HINTS = (
    "not supported", "unsupported", "unknown parameter", "unrecognized",
    "invalid parameter", "unknown field", "extra field", "not allowed",
)


# 参数协商结果记忆(进程内, 键=(apibase, model))——2026-09-13 实测新增。
# 上游队列的参数能力在进程生命周期内不会变, 但每个请求都重新"撞一次 400 再裁剪"
# 毫无意义: 实测一次任务可调 20+ 次生图, 每次都白付 ~1.3s + 一条上游 400
# (llm-proxy WARN 噪音 + 上游侧失败计数)。只记装饰性参数(_IMAGE_GEN_TRIMMABLE),
# 故记忆失效的代价仅是"少发一个可选参数", 不影响请求语义与交付契约。
_IMAGE_GEN_TRIM_MEMO = {}
_IMAGE_GEN_TRIM_MEMO_MAX = 64  # 防御长命进程(CLI/桌面端)无界增长


def _image_gen_memo_key(api_base, model):
    return (str(api_base or ''), str(model or ''))


def _image_gen_remember_trim(api_base, model, names):
    """记住"该 (网关, 模型) 不支持这些装饰性参数", 后续请求直接不发。"""
    if not names:
        return
    key = _image_gen_memo_key(api_base, model)
    known = _IMAGE_GEN_TRIM_MEMO.setdefault(key, set())
    known.update(names)
    while len(_IMAGE_GEN_TRIM_MEMO) > _IMAGE_GEN_TRIM_MEMO_MAX:
        _IMAGE_GEN_TRIM_MEMO.pop(next(iter(_IMAGE_GEN_TRIM_MEMO)))


# 图片容器魔数: 上游可能裁剪/忽略 output_format(实测 agnes 不接受该参数),
# 交付文件扩展名必须跟随真实字节, 否则 IM 侧 MIME 失配(§6.5 失败诚实)。
def _image_gen_data_url(raw, mime):
    """把参考图编码成 Data-URL。部分上游(如 SenseNova)只接受公网 URL 或带
    `data:image/*;base64,` 前缀的 Data-URL,**不接受纯 base64 字符串**; 我们没有公网图床,
    因此统一用 Data-URL(官方文档明示支持)。"""
    return f"data:{mime or 'image/png'};base64," + base64.b64encode(raw).decode('ascii')


def sniff_image_format(data):
    """按魔数识别图片容器格式, 返回 'png'/'jpeg'/'gif'/'webp', 未知返回 None。"""
    if not data or len(data) < 12:
        return None
    if data.startswith(b"\x89PNG\r\n\x1a\n"):
        return "png"
    if data.startswith(b"\xff\xd8\xff"):
        return "jpeg"
    if data.startswith(b"GIF87a") or data.startswith(b"GIF89a"):
        return "gif"
    if data.startswith(b"RIFF") and data[8:12] == b"WEBP":
        return "webp"
    return None


# ═══════════════════════════════════════════════════════════════════════════
# 通道能力档案（2026-09-14；设计真值 .tasks/image-capability/DESIGN.zh-CN.md §8）
#
# 为什么是数据、不是代码分支：本渠道实测有 11 个图像模型（GET /v1/models），端点与参数
# 支持各不相同。把"可裁剪参数名 + 错误话术 + 模型名匹配"写进代码 = 每接一个模型改一次
# 代码，且第一次请求必然白付一次 400。成熟做法一致（pi 的 ImagesModel.api +
# images-api-registry、OpenRouter 的 supported_parameters、LiteLLM 的
# get_supported_openai_params）：**逐通道声明能力**，不支持的参数默认报错，静默丢弃是
# 显式 opt-in。
#
# 不变量（红线，测试钉住）：
#   语义参数（prompt/n/size/image/aspect_ratio/resolution）—— 改了等于改用户意图，
#     通道不支持 → fail-loud，绝不静默缩水；
#   装饰参数（output_format/quality/seed/stream/partial_images）—— 缺席时上游给合理
#     默认，通道不支持 → 省略，但必须在工具结果里明示省略了什么。
# ═══════════════════════════════════════════════════════════════════════════
# 注：`aspect_ratio`/`resolution` 是**保留的语义参数**（主流形态，如 OpenRouter 的
# aspect_ratio/resolution、Gemini 的 responseFormat.image）——本网关把 ratio 静默丢弃
# （§2），工具暂未暴露它们，故档案里不声明；将来暴露时只需填档案 `maps`，不改控制流。
_IMAGE_SEMANTIC_PARAMS = ('prompt', 'n', 'size', 'image', 'aspect_ratio', 'resolution')
_IMAGE_DECORATIVE_PARAMS = ('output_format', 'quality', 'seed', 'stream', 'partial_images')

# size 的**形态**是逐通道事实（2026-09-14 实测）：agnes 接受档位 1K/2K 与原生像素；
# gemini/gpt-image/sensenova 只接受 WxH——传档位会被网关回「图片尺寸格式错误，应为 宽x高，
# 例如 1024x1024」，而且该错误被标成 **500** 而不是 400。size 是语义参数 → 形态不符必须
# fail-loud（不静默替换用户要求的尺寸）。
_IMAGE_SIZE_TIER_RE = re.compile(r'^\s*\d+\s*[kK]\s*$')
_IMAGE_SIZE_PIXELS_RE = re.compile(r'^\s*\d+\s*[xX*×]\s*\d+\s*$')

_IMAGE_API_GENERATIONS = 'images_generations'
_IMAGE_API_EDITS = 'images_edits'
_IMAGE_API_EDITS_JSON = 'images_edits_json'
_IMAGE_APIS = (_IMAGE_API_GENERATIONS, _IMAGE_API_EDITS, _IMAGE_API_EDITS_JSON)
_IMAGE_EDIT_APIS = (_IMAGE_API_EDITS, _IMAGE_API_EDITS_JSON)


def _deco(**supported):
    """装饰参数声明表：**未列出的装饰参数一律声明为不支持(None)** → 省略 + 明示。
    档案是权威的：新增装饰参数必须同时更新档案（宁可不发，也不发出去撞 400）。"""
    table = {p: None for p in _IMAGE_DECORATIVE_PARAMS}
    table.update(supported)
    return table


# maps: 统一参数 → 线参数名（None = 该通道不支持，
#       不管是“实测被拒”还是“未验证”——两者对请求构造的含义相同：不发；差异写在 evidence 里）
_IMAGE_CATALOG = {
    # ── 本渠道实测（newapi.myovo.cc.cd，2026-09-13/14；能力矩阵见 DESIGN §2） ──
    'agnes-image-2.5-flash': dict(
        api=_IMAGE_API_GENERATIONS, ops={'generate'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 0},
        size_style='both',                   # 实测档位 1K/2K 与原生像素均通过
        latency='fast', cost='metered', source='measured',
        evidence='文生图 200/8-11s；text image queue 实测拒收 output_format/quality(400)；'
                 'ratio/extra_body 静默丢弃。**改图不声明（证据矛盾，宁可不做）**：'
                 '2026-09-14 探针显示 multipart edits 路由回 image is required（仅说明网关适配器在），'
                 '但 2026-08-14 真实改图实测 503/106s 且 Agnes 官方无 /images/edits 端点；'
                 'JSON edits 路径 30s 无响应。未证实不声明 → 带参考图走本通道会 fail-closed',
    ),
    'agnes-image-2.1-flash': dict(
        api=_IMAGE_API_GENERATIONS, ops={'generate'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 0},
        size_style='both',
        latency='fast', cost='metered', source='measured',
        evidence='与 2.5 同队列（2026-08-14 实测：只回 url 直链；2026-09-14 探测：档位通过 size 校验）',
    ),
    'agnes-image-2.0-flash': dict(
        api=_IMAGE_API_GENERATIONS, ops={'generate'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 0},
        size_style='both',
        latency='fast', cost='metered', source='measured',
        evidence='2026-09-14 探测：档位 1K 通过 size 校验、只报 prompt is required（endpoint+模型可用）',
    ),
    'sensenova-u1.5-lite': dict(
        api=_IMAGE_API_EDITS_JSON, ops={'generate', 'edit'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 1, 'max_refs': 5},   # 2026-09-14 探测：网关校验 images 为 1..5 项
        extra_params={'watermark': False},
        latency='slow', cost='free', source='measured',
        evidence='改图 JSON 200/49.6s 像素验证（免费；2026-09-14 生产路径复验 200/70.8s/888KB，'
                 '中心像素蓝→红）；文生图 1024x1024 复验 200/42.9s；n 只能 1；2K 文生图撞 CF 524；'
                 'read_timeout 必须 ≥110s（慢）；'
                 '2026-09-14 探测：size 只接受 WxH（auto 或 32 倍数/512-4096/比例≤3:1），'
                 '参考图 1..5 张（网关原文 invalid images, should contain between 1 and 5 items）',
    ),
    'sensenova-u1-fast': dict(
        api=_IMAGE_API_GENERATIONS, ops={'generate'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 1, 'max_refs': 5},
        latency='slow', cost='free', source='measured',
        evidence='2026-08-14 实测只回 url 直链（b64 空）；2026-09-14 探测：文生图端点 400 '
                 'field Prompt invalid；JSON edits 端点 400 invalid images, should contain between 1 and 5 items',
    ),
    'gemini-3.1-flash-image': dict(
        api=_IMAGE_API_EDITS, ops={'generate', 'edit'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='fast', cost='paid', source='measured',
        evidence='/images/edits multipart 200/9.0s 像素验证；返回 JPEG（魔数嗅探改名）；'
                 '2026-09-14 探测：size 只接受 WxH（传 1K 被网关回「图片尺寸格式错误」且标成 500）；'
                 '/images/edits 路由+适配器已探活（image is required），未做真图验证',
    ),
    # ── 端点/操作按网关源码与官方文档声明，逐模型参数未实测（故装饰参数保守） ──
    'gemini-3.1-flash-image-preview': dict(
        api=_IMAGE_API_EDITS, ops={'generate', 'edit'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='fast', cost='paid', source='documented',
        evidence='Gemini 官方：生成/编辑同一入口；预览版未在本网关实测',
    ),
    'gemini-3-pro-image': dict(
        api=_IMAGE_API_EDITS, ops={'generate', 'edit'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='slow', cost='paid', source='documented',
        evidence='Gemini 官方：Pro 版支持 4K/多参考图（本网关未实测）',
    ),
    'gemini-3-pro-image-preview': dict(
        api=_IMAGE_API_EDITS, ops={'generate', 'edit'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='slow', cost='paid', source='documented',
        evidence='同 gemini-3-pro-image（预览通道）',
    ),
    'gemini-3.0-pro-image-preview': dict(
        api=_IMAGE_API_EDITS, ops={'generate', 'edit'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='slow', cost='paid', source='documented',
        evidence='同 gemini-3-pro-image（3.0 预览通道）；2026-09-14 探测：文生图端点可达'
                 '（500 prompt is required）、edits JSON 路径回 image is required，size 只接受 WxH',
    ),
    'gpt-image-2': dict(
        api=_IMAGE_API_EDITS, ops={'generate', 'edit'},
        maps={**_deco(output_format='output_format', quality='quality',
                      stream='stream', partial_images='partial_images'),
              'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='slow', cost='paid', source='documented',
        evidence='2026-08-14 实测接受 output_format；2026-09-13 实测 24-125s 在 CF 120s 窗口下会撞 524；'
                 '2026-09-14 探测：size 只接受 WxH、/images/edits 路由+适配器已探活（image is required），未做真图验证',
    ),
    'gpt-image-2.5-flare': dict(
        api=_IMAGE_API_EDITS, ops={'generate', 'edit'},
        maps={**_deco(output_format='output_format', quality='quality',
                      stream='stream', partial_images='partial_images'),
              'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='slow', cost='paid', source='documented',
        evidence='与 gpt-image-2 同族（本网关未单独实测）',
    ),
    # ── OpenAI 官方模型（直连形态用；本渠道没有这些型号） ──
    'gpt-image-1': dict(
        api=_IMAGE_API_GENERATIONS, ops={'generate'},
        maps={**_deco(output_format='output_format', quality='quality',
                      stream='stream', partial_images='partial_images'),
              'size': 'size', 'n': 'n'},
        limits={'max_n': 4, 'max_refs': 1},
        latency='slow', cost='metered', source='documented',
        evidence='OpenAI 官方：恒返回 b64_json（发 response_format 会 400，见二轮审查 I-3）',
    ),
    'dall-e-3': dict(
        api=_IMAGE_API_GENERATIONS, ops={'generate'},
        maps={**_deco(quality='quality'), 'size': 'size', 'n': 'n'},
        limits={'max_n': 1, 'max_refs': 0},
        fixed={'response_format': 'b64_json'},
        latency='slow', cost='metered', source='documented',
        evidence='OpenAI 官方：无 output_format 概念/无 stream；n 只能 1',
    ),
    'dall-e-2': dict(
        api=_IMAGE_API_GENERATIONS, ops={'generate'},
        maps={**_deco(), 'size': 'size', 'n': 'n'},
        limits={'max_n': 10, 'max_refs': 0},
        fixed={'response_format': 'b64_json'},
        latency='slow', cost='metered', source='documented',
        evidence='OpenAI 官方：无 output_format/quality 概念',
    ),
}

# 未建档通道（catalog 无此模型且配置未声明）: 宽松集 + 有界自愈 + 显式标注未验证。
_IMAGE_PERMISSIVE_MAPS = {p: p for p in _IMAGE_DECORATIVE_PARAMS}
_IMAGE_PERMISSIVE_MAPS.update({'size': 'size', 'n': 'n'})


class ImageChannelProfile:
    """一条通道（网关 × 模型）的能力档案：**数据**，不是代码分支。

    字段：
      api      —— 线协议（端点与传输形态）
      ops      —— 本通道实际能做的操作（generate/edit）
      maps     —— 统一参数 → 线参数名；None = 声明为不支持（**唯一表示**，不再另设"被拒集合"）
      limits   —— max_n / max_refs（**语义上限**，超限 fail-loud，不静默缩水）
      fixed    —— 每次请求必带的固定参数（如 dall-e 的 response_format）
      extra_params —— 运营声明的透传参数（不得覆盖语义字段）
      source   —— measured | documented | config | unverified（决定宽松/严格与提示文案）
      evidence —— 结论依据（哪次实测/哪份文档），排障与复查用
    """

    def __init__(self, api, ops, maps=None, limits=None, fixed=None, extra_params=None,
                 latency='unknown', cost='unknown', source='unverified', evidence='',
                 size_style='pixels'):
        # size_style: 'pixels'(WxH) | 'tier'(1K/2K) | 'both'。catalog 条目默认按主流
        # （WxH）严格声明；未建档通道在 resolve_image_profile 里放宽为 'both'。
        self.size_style = size_style
        self.api = api
        self.ops = set(ops or ())
        self.maps = dict(maps or {})
        self.limits = dict(limits or {})
        self.fixed = dict(fixed or {})
        self.extra_params = dict(extra_params or {})
        self.latency = latency
        self.cost = cost
        self.source = source
        self.evidence = evidence

    @classmethod
    def from_catalog(cls, model, host=''):
        """从内置 catalog 取该模型（可选用 `<host>|<model>` 网关限定键覆盖）的档案。
        返回 None = 未建档（调用方按宽松集 + 未验证处理）。"""
        host_key = f"{host}|{model}" if host else ''
        entry = (_IMAGE_CATALOG.get(host_key) if host_key else None) or _IMAGE_CATALOG.get(str(model or ''))
        return cls(**entry) if entry else None

    @property
    def max_n(self):
        return max(1, int(self.limits.get('max_n', 1) or 1))

    @property
    def max_refs(self):
        return max(0, int(self.limits.get('max_refs', 0) or 0))

    @property
    def verified(self):
        return self.source in ('measured', 'documented', 'config')

    def supports(self, operation):
        return operation in self.ops

    def wire_key(self, param):
        """统一参数 → 线参数名；None = 档案声明不支持（调用方按语义/装饰二分处理）。"""
        if param in self.maps:
            return self.maps[param]
        # 档案未提及的参数：严格档案按“不支持”处理（宁可不发），未建档通道宽松发送
        return None if self.verified else param

    def op_label(self, zh=True):
        names = [('文生图' if zh else 'generate'), ('改图' if zh else 'edit')]
        return '+'.join(n for n, op in zip(names, ('generate', 'edit')) if op in self.ops) or ('无' if zh else 'none')

    def cost_label(self, zh=True):
        return {'free': ('免费' if zh else 'free'),
                'paid': ('按张付费' if zh else 'paid per image'),
                'metered': ('计量收费' if zh else 'metered')}.get(self.cost, self.cost)

    def describe(self, model, host='', zh=True):
        """档案 → 能力文本（工具 schema 渲染用；同一份数据，不是第二处手写文案）。"""
        where = f"{model}@{host}" if host else str(model)
        refs = ('参考图: 不支持' if zh else 'reference images: unsupported') if not self.max_refs else (
            f"参考图: 最多 {self.max_refs} 张" if zh else f"reference images: up to {self.max_refs}")
        size_form = {'both': ('档位(1K/2K)与 WxH 像素均可' if zh else 'tiers (1K/2K) or WxH pixels'),
                     'tier': ('只接受档位(1K/2K)' if zh else 'tiers (1K/2K) only'),
                     'pixels': ('只接受 WxH 像素(如 1024x1024)' if zh else 'WxH pixels only')}.get(
                         self.size_style, self.size_style)
        line = (f"model={where} ｜ 能力: {self.op_label(zh)} ｜ {refs} ｜ "
                f"size 形态: {size_form} ｜ "
                f"{'单次张数上限' if zh else 'max n'}: {self.max_n} ｜ "
                f"{'计费' if zh else 'cost'}: {self.cost_label(zh)} ｜ "
                f"{'典型耗时' if zh else 'latency'}: {self.latency}")
        dropped = sorted(p for p, wire in self.maps.items()
                         if wire is None and p in _IMAGE_DECORATIVE_PARAMS)
        if dropped:
            line += (f" ｜ {'已知不支持(自动省略)' if zh else 'known-unsupported (auto-omitted)'}: {', '.join(dropped)}")
        if not self.verified:
            line += (' ｜ 该模型未建档(能力未验证): 参数按宽松集发送, 上游拒绝时自动裁剪'
                     if zh else ' ｜ unlisted model (unverified): permissive params + bounded self-heal')
        return line

    def proposal(self, names, api_base='', model=''):
        """协商学到的结论 → 可直接粘进 catalog 的补丁（把运行时发现固化成数据）。"""
        patch = {str(n): None for n in names}
        return {'key': f"{model}", 'where': str(api_base or ''), 'maps_patch': patch}


def resolve_image_profile(model, cfg):
    """档案解析（优先级：配置显式声明 > 内置 catalog > 未建档宽松集）。

    - catalog 里且 `verified` 的条目是**权威**：未提及的参数 = 不支持（宁可不发）。
    - catalog 无此模型（或条目未验证/配置声明 unverified）→ 宽松集 + 有界自愈，
      但档案被显式标为 `unverified`，工具能把这一点如实告诉模型。
    - 显式声明（protocol/operations/profile/unverified）优先于内置档案：运营最懂自己的网关。"""
    cfg = cfg or {}
    host = str(cfg.get('apibase', '')).split('//')[-1].split('/')[0].lower()
    cat = ImageChannelProfile.from_catalog(model, host)
    explicit = cfg.get('profile') or {}
    if not isinstance(explicit, dict):
        raise ValueError('image_gen: profile 必须是 dict')
    api = str(cfg.get('protocol') or explicit.get('api')
              or (cat.api if cat else _IMAGE_API_GENERATIONS)).strip().lower()
    if api not in _IMAGE_APIS:
        raise ValueError(f"image_gen: 不支持的 protocol {api!r} (支持 {', '.join(_IMAGE_APIS)})")
    declared_ops = explicit.get('ops') or cfg.get('operations')
    if declared_ops:
        ops = {str(op).strip().lower() for op in declared_ops if str(op).strip()}
    elif cat:
        ops = set(cat.ops)
    else:
        ops = {'edit'} if api in _IMAGE_EDIT_APIS else {'generate'}
    unknown_ops = ops - {'generate', 'edit'}
    if unknown_ops:
        raise ValueError(f"image_gen: operations 只支持 generate/edit, 收到 {sorted(unknown_ops)}")
    unverified = bool(cfg.get('unverified') or explicit.get('unverified'))
    if cat and cat.verified and not unverified:
        source = 'config' if (cfg.get('protocol') or cfg.get('operations') or explicit) else cat.source
        maps = dict(cat.maps)
    else:
        source = 'unverified'
        maps = dict(_IMAGE_PERMISSIVE_MAPS)
    maps.update(explicit.get('maps') or {})
    size_style = str(explicit.get('size_style') or (cat.size_style if cat else 'both')).strip().lower()
    if size_style not in ('pixels', 'tier', 'both'):
        raise ValueError(f"image_gen: size_style 只支持 pixels/tier/both, 收到 {size_style!r}")
    limits = {'max_n': 4, 'max_refs': int(BaseImageGenClient.MAX_EDIT_IMAGES)}
    if cat:
        limits.update(cat.limits)
    limits.update(explicit.get('limits') or {})
    fixed = dict(cat.fixed) if cat else {}
    fixed.update(explicit.get('fixed') or {})
    extra = dict(cat.extra_params) if cat else {}
    extra.update(cfg.get('extra_params') or {})
    return ImageChannelProfile(
        api=api, ops=ops, maps=maps, limits=limits, fixed=fixed, extra_params=extra,
        latency=cat.latency if cat else 'unknown', cost=cat.cost if cat else 'unknown',
        source=source, evidence=str(explicit.get('evidence') or (cat.evidence if cat else '')),
        size_style=size_style,
    )


class BaseImageGenClient:
    """生图客户端基类。只解析生图所需配置子集
    {apibase/apikey/model/protocol/operations/stream/timeout/read_timeout/max_retries/proxy/verify},
    不照抄 BaseSession 的 chat 专属字段(context_win/thinking 等)。

    **protocol / operations（2026-09-13 实测新增，见 .tasks/image-capability/DESIGN.zh-CN.md）**：
    "能否改图"是**通道属性**（上游端点 + 网关转发行为），不是模型属性：
      - `protocol="images_generations"`（默认）：JSON POST `{apibase}/images/generations`（文生图）；
      - `protocol="images_edits"`：multipart POST `{apibase}/images/edits`，文件字段 `image`（参考图/改图）；
    `operations` 声明本通道实际能做什么（缺省由 protocol 推导：edits→{'edit'}、generations→{'generate'}）。
    **调用未声明的 operation 一律 fail-closed 报错**——因为存在"网关照文档收下参数但静默丢弃"的通道
    （实测 agnes 的 `extra_body.image` 就是静默无效），宁可知情失败，不可假装成功。"""

    # 线协议常量与模块级 _IMAGE_APIS 同源（档案与客户端共用一份真值）
    PROTOCOL_GENERATIONS = _IMAGE_API_GENERATIONS
    PROTOCOL_EDITS = _IMAGE_API_EDITS
    # 2026-09-13 实测新增: 部分上游(如 SenseNova U1.5 Lite)的改图接口是 **JSON**,
    # 图片用 `images:[{image_url: <公网URL|data:image/*;base64,...>}]` 传入——
    # 与 OpenAI 的 multipart 文件上传完全不同(发 multipart 会被上游回 `invalid arguments`)。
    # 官方依据: platform.sensenova.cn/docs → SenseNova U1.5 Lite → 图片编辑接口。
    PROTOCOL_EDITS_JSON = _IMAGE_API_EDITS_JSON
    _PROTOCOLS = _IMAGE_APIS
    _EDIT_PROTOCOLS = _IMAGE_EDIT_APIS
    # 参考图单张预算(防内存/上游爆): ≤8MiB。
    # **为何默认只支持 1 张**(2026-09-13 源码依据, 不猜): new-api `relay/helper/valid_request.go`
    # 的 edits 分支只读 `formData.Get("image")`(单值); SenseNova 的 `images` 数组虽是多图形态,
    # 但多图在**本网关 + 路由**上未经实测 → 宁可知情拒绝, 不可静默只取第一张。
    # 上限的**真值在档案** (`limits.max_refs`), 此常量仅为未声明上限时的默认值。
    MAX_EDIT_IMAGES = 1
    MAX_EDIT_IMAGE_BYTES = 8 * 1024 * 1024
    # extra_params 不得覆盖的语义字段(防“配置静默改写用户意图”)
    _PROTECTED_PAYLOAD_KEYS = ('model', 'prompt', 'n', 'images', 'image')

    def __init__(self, cfg):
        self.name = cfg.get('name', 'image_gen')
        self.api_key = cfg.get('apikey', '')
        self.api_base = str(cfg.get('apibase', '')).rstrip('/')
        self.model = cfg.get('model', '')
        if not self.api_base or not self.api_key or not self.model:
            raise ValueError('image_gen 配置不完整: 需要 apibase/apikey/model')
        self._cfg = dict(cfg)
        # extra_params 不得覆盖语义字段(防"配置静默改写用户意图")
        extra = cfg.get('extra_params') or {}
        if not isinstance(extra, dict):
            raise ValueError('image_gen: extra_params 必须是 dict')
        bad = [k for k in extra if k in self._PROTECTED_PAYLOAD_KEYS]
        if bad:
            raise ValueError(f"image_gen: extra_params 不得覆盖语义参数 {', '.join(sorted(bad))}")
        self._profiles = {}                     # model → ImageChannelProfile(含 per-request 覆写)
        prof = self.profile_for(self.model)          # 校验 protocol/operations 取值(非法立即抛 ValueError)
        self.protocol = prof.api                # 兼容属性: 基模型的线协议
        self.operations = set(prof.ops)         # 兼容属性: 基模型声明的操作
        self.extra_params = dict(prof.extra_params)
        self.last_notices = []                  # 本次请求被省略/裁剪的参数 [(名, 原因)], ga 层明示给模型
        self.stream = bool(cfg.get('stream', False))
        self.max_retries = max(0, int(cfg.get('max_retries', 2)))
        self.max_retry_after = float(cfg.get('max_retry_after', 60.0))
        self.connect_timeout = max(1, int(cfg.get('timeout', 10)))
        self.read_timeout = max(5, int(cfg.get('read_timeout', 120)))
        proxy = cfg.get('proxy')
        self.proxies = {"http": proxy, "https": proxy} if proxy else None
        self.verify = cfg.get('verify', True)
        self.http = _build_http_session()

    def profile_for(self, model=None):
        """取该模型的能力档案(带缓存)。模型决定能力: 请求里覆写 model 会切到对应档案。"""
        name = str(model or self.model)
        if name not in self._profiles:
            self._profiles[name] = resolve_image_profile(name, self._cfg)
        return self._profiles[name]

    @property
    def profile(self):
        """基模型的能力档案(请求级覆写请用 profile_for(model))。"""
        return self.profile_for(self.model)

    def _supported_hint(self, profile):
        return ', '.join(p for p in ('prompt', 'size', 'n', 'image') if profile.wire_key(p)) or '无'

    def _unsupported_semantic(self, profile, model, param, detail=''):
        """语义参数不支持 → fail-loud(附该通道实际支持集, 让模型能改参自愈)。"""
        return (f"[Error: image_gen 通道({model}) 未声明支持 {param}{detail}——"
                f"{param} 是语义参数(改了等于改用户意图), 不做静默缩水; "
                f"该通道支持: {self._supported_hint(profile)}, 可用操作: {profile.op_label(zh=False)}")

    def _endpoint(self, operation='generate', api=None):
        # 端点由 **operation + 档案的 api** 决定(OpenAI 语义两端点), api 只描述"改图走什么传输":
        #   generate → JSON POST /images/generations(所有通道通用)
        #   edit     → images_edits(multipart, OpenAI 官方形态) 与 images_edits_json
        #              (JSON + images[{image_url}], 如 SenseNova)都打 /images/edits
        # 其它 edits 传输形态(如豆包 seedream 把改图走 generations JSON, 见 new-api PR
        # #2090)属**未验证扩展点**, 需要时再加 api 取值 + 实测, 不提前实现。
        api = api or self.protocol
        if operation == 'edit' and api in self._EDIT_PROTOCOLS:
            return auto_make_url(self.api_base, 'images/edits')
        return auto_make_url(self.api_base, 'images/generations')

    def _headers(self, multipart=False):
        headers = {"Authorization": f"Bearer {self.api_key}", "Accept": "application/json"}
        if not multipart:
            headers["Content-Type"] = "application/json"
        # multipart 时不能自己设 Content-Type: requests 要写入含 boundary 的头
        return headers

    def _build_payload(self, profile, prompt, size=None, quality=None, n=None,
                       output_format=None, model=None, stream=None):
        """按**档案**构造请求体: 只发档案声明支持的参数(构造期裁剪, 不是发出去再裁)。

        返回 (payload|None, notices, err):
          notices —— 本次被省略的装饰参数(可省, 但必须明示给模型/用户);
          err     —— 语义参数不被支持(fail-loud, 不静默缩水)。"""
        model_eff = str(model or self.model)
        notices = []
        payload = {'model': model_eff, 'prompt': prompt, 'n': int(n or 1)}
        # size: 协议必传(new-api 图片计费按 size 校验) + 语义参数 → 不支持则如实报错
        wire_size = profile.wire_key('size')
        if wire_size is None:
            return None, notices, self._unsupported_semantic(profile, model_eff, 'size')
        # new-api 类中转实测(2026-08-14): size 必传(图片尺寸计费), 缺省会
        # 500 "图片尺寸计费需要传 size"。默认 1024x1024 = OpenAI 官方默认值。
        size_req = str(size or "1024x1024")
        # size 的形态逐通道不同(2026-09-14 实测): 形态不符如实报错, 不静默替换用户要求的尺寸
        if profile.size_style == 'pixels' and not _IMAGE_SIZE_PIXELS_RE.match(size_req):
            return None, notices, (f"[Error: image_gen 通道({model_eff}) 的 size 只接受 WxH 像素"
                                   f"(实测该网关对档位回「图片尺寸格式错误，应为 宽x高，例如 1024x1024」); "
                                   f"收到 {size_req!r}——size 是语义参数, 不静默替换; 请传如 1024x1024 / 1536x1024")
        if profile.size_style == 'tier' and not _IMAGE_SIZE_TIER_RE.match(size_req):
            return None, notices, (f"[Error: image_gen 通道({model_eff}) 的 size 只接受档位(1K/2K); "
                                   f"收到 {size_req!r}——size 是语义参数, 不静默替换")
        payload[wire_size] = size_req
        # 装饰参数: 档案声明支持才发; 不支持的省略并明示(不静默)
        #   output_format: png 是协议默认值 → 不发(显式发 png 只是多一个上游 400 借口)
        for name, value in (('quality', quality),
                            ('output_format', output_format if output_format and output_format != 'png' else None),
                            ('stream', True if stream else None)):
            if value is None:
                continue
            wire = profile.wire_key(name)
            if wire is None:
                notices.append((name, f"该通道未声明支持 {name}，已省略（不影响请求语义）"))
                continue
            payload[wire] = value
        payload.update(profile.fixed)          # 如 dall-e 的 response_format=b64_json
        # 已协商过的"该网关+模型不支持"装饰参数不再重复发送(去掉每次一次白 400)。
        for name in _IMAGE_GEN_TRIM_MEMO.get(_image_gen_memo_key(self.api_base, model_eff), ()):
            payload.pop(name, None)
        # 档案声明的透传参数(如 SenseNova watermark=false): 内置档案里的是可信代码常量,
        # 不得覆盖语义字段(否则等于档案静默改写用户意图)
        for key, value in profile.extra_params.items():
            if key in self._PROTECTED_PAYLOAD_KEYS:
                raise ValueError(f"image_gen: 档案 extra_params 不得覆盖语义参数 {key}")
            payload[key] = value
        return payload, notices, None

    def _delay(self, resp, attempt):
        """仿 _stream_with_retry 退避: retry-after 头优先, 超上限不重试;
        否则指数退避 1.5*2^attempt, 夹在 [0.5, 30]s。
        注意底数 _IMG_BACKOFF_BASE=1.5 与主链路 _BACKOFF_BASE=3.0 不同。"""
        try:
            ra = float((resp.headers or {}).get("retry-after"))
        except (TypeError, ValueError):
            ra = None
        # 与 _stream_with_retry(447-487) 完全一致: retry-after=0 时也走指数退避。
        return None if ra is not None and ra > self.max_retry_after else max(_BACKOFF_MIN, ra or min(_BACKOFF_CAP, _IMG_BACKOFF_BASE * (2 ** attempt)))

    def _post(self, payload, stream=False, files=None, operation='generate', api=None):
        """带重试语义的 POST + 参数协商自愈。

        单轮语义见 _post_once(429/408/5xx 退避集合 + retry-after 上限)。
        外层负责"上游不支持某参数"的自愈: 400/422 且错误文本点名了本次请求
        里的可裁剪参数 → 裁剪后重试(独立预算, 不消耗 max_retries)。
        files 非空时改走 multipart(改图): payload 作为普通表单字段发送。

        返回 (resp|dict|None, err_text), 错误文本统一 [Error: image_gen ...]
        前缀(§6.5, 绝不用 !!!Error:)。stream=True 成功时返回打开的响应对象
        (调用方负责 close)。"""
        payload = dict(payload)
        trims = 0
        conservative_done = False
        while True:
            out, err, err_body, err_status = self._post_once(payload, stream=stream, files=files,
                                                              operation=operation, api=api)
            if err is None:
                return out, None
            if err_status in _IMAGE_GEN_PARAM_TRIM_STATUS and trims < _IMAGE_GEN_MAX_PARAM_TRIMS:
                name = self._unsupported_param(err_body, payload)
                if name:
                    payload.pop(name, None)
                    trims += 1
                    _image_gen_remember_trim(self.api_base, payload.get("model"), [name])
                    self.last_notices.append(
                        (name, "上游拒绝该参数，已自动裁剪（建议把 profile-proposal 固化进档案）"))
                    print(f"[ImageGen Adapt] 上游不支持参数 {name!r}, 已裁剪后重试: {err_body[:160]}")
                    self._print_profile_proposal([name], payload.get("model"))
                    continue
                # 错误文本不可判读(网关清洗/非标准话术): 只要还带着装饰性参数,
                # 就退回"保守参数集"重试**一次**——不依赖上游话术, 使托管形态
                # (llm-proxy 为安全边界默认清洗上游错误体, 见 reverse_proxy.go)
                # 也能自愈。只做一次, 且只丢 _IMAGE_GEN_TRIMMABLE(纯装饰性,
                # 不影响请求语义与交付契约), 故不会把内容/鉴权类失败变成无限重试。
                if not conservative_done:
                    dropped = [k for k in _IMAGE_GEN_TRIMMABLE if k in payload]
                    if dropped:
                        for k in dropped:
                            payload.pop(k, None)
                        conservative_done = True
                        _image_gen_remember_trim(self.api_base, payload.get("model"), dropped)
                        self.last_notices.append(
                            (', '.join(dropped), "4xx 错误体不可判读（疑被网关清洗）→ 退回保守参数集"))
                        print(f"[ImageGen Adapt] 4xx 不可判读(疑被网关清洗), 退回保守参数集(丢弃 {', '.join(dropped)})重试一次: {err_body[:160]}")
                        self._print_profile_proposal(dropped, payload.get("model"))
                        continue
            # 预算用尽/无可裁剪参数 → 如实返回上游错误(不返回合成"耗尽"文本,
            # 保留上游 message 供模型自愈改参)。
            return None, err

    def _print_profile_proposal(self, names, model):
        """把协商学到的结论打一行**可直接固化进 catalog** 的补丁（运行时发现 → 数据）。"""
        prop = self.profile_for(model).proposal(names, self.api_base, model)
        print(f"[ImageGen Adapt] profile-proposal: {json.dumps(prop['maps_patch'], ensure_ascii=False)}"
              f"  # key={prop['key']} where={prop['where']}")

    def _post_once(self, payload, stream=False, files=None, operation='generate', api=None):
        """单轮 POST(仿 _stream_with_retry: 429/408/5xx 退避集合 + retry-after
        上限)。返回 (resp|dict|None, err_text, err_body, err_status); err_body
        仅供参数协商解析(非 4xx 时为 "")。files 非空 → multipart。"""
        url = self._endpoint(operation, api)
        headers = self._headers(multipart=bool(files))
        # multipart 的"参数"在表单字段里, 协商裁剪同样作用于 payload(表单字段)
        send = {"data": dict(payload), "files": files} if files else {"json": dict(payload)}
        for attempt in range(self.max_retries + 1):
            resp = None
            try:
                resp = self.http.post(url, headers=headers, stream=stream,
                                      timeout=(self.connect_timeout, self.read_timeout),
                                      proxies=self.proxies, verify=self.verify, **send)
                if resp.status_code >= 400:
                    body = ""
                    try:
                        body = resp.text.strip()[:500]
                    except Exception:
                        pass
                    status = resp.status_code
                    d = None
                    if status in _IMAGE_GEN_RETRYABLE and attempt < self.max_retries:
                        d = self._delay(resp, attempt)
                        if d is not None:
                            print(f"[ImageGen Retry] HTTP {status}, retry in {d:.1f}s ({attempt+1}/{self.max_retries+1})")
                            time.sleep(d)
                            resp.close()
                            resp = None
                            continue
                    hint = " (retry-after > cap)" if status in _IMAGE_GEN_RETRYABLE and d is None and attempt < self.max_retries else ""
                    resp.close()  # stream=True 的 4xx 响应也要关(此前只关非流式)
                    resp = None
                    return None, f"[Error: image_gen HTTP {status}{hint}" + (f": {body}" if body else "") + "]", body, status
                if stream:
                    return resp, None, "", None
                try:
                    return resp.json(), None, "", None
                except ValueError:
                    return None, "[Error: image_gen 响应不是合法 JSON]", "", None
            except (requests.Timeout, requests.ConnectionError, requests.exceptions.ChunkedEncodingError) as e:
                if attempt < self.max_retries:
                    d = self._delay(None, attempt)
                    print(f"[ImageGen Retry] {type(e).__name__}, retry in {d:.1f}s ({attempt+1}/{self.max_retries+1})")
                    time.sleep(d)
                    continue
                return None, (f"[Error: image_gen {type(e).__name__}: {e}]" if str(e) else f"[Error: image_gen {type(e).__name__}]"), "", None
            except Exception as e:
                return None, f"[Error: image_gen {type(e).__name__}: {e}]", "", None
            finally:
                if resp is not None and not stream:
                    resp.close()
        return None, "[Error: image_gen 重试耗尽]", "", None

    def _unsupported_param(self, body, payload):
        """从 4xx 错误文本里解析"上游不支持的参数名", 仅在同时满足三条时返回:
        ① 文本命中"参数不被支持/未知参数"话术族; ② 文本里字面出现该参数名;
        ③ 该参数确在本次 payload 中且属于 _IMAGE_GEN_TRIMMABLE。

        三条同时成立才裁剪, 是为了不误裁无关 4xx(如 prompt 缺失、鉴权失败)。
        实测上游话术(2026-09-13, new-api 中转 → agnes-image-2.5-flash):
          "output_format is not supported by text image queue"
          "quality is not supported by text image queue"
        兼容 OpenAI 系话术: "Unknown parameter: 'x'" / "Unrecognized request
        argument supplied: x" / "Unsupported parameter: x"。"""
        low = (body or "").lower()
        if not low:
            return None
        if not any(h in low for h in _IMAGE_GEN_PARAM_UNSUPPORTED_HINTS):
            return None
        for name in _IMAGE_GEN_TRIMMABLE:
            if name in payload and name in low:
                return name
        return None

    def _extract_images(self, data):
        """从同步响应 {data:[{b64_json|url}]} 提取 bytes 列表。
        优先 b64_json(OpenAI/gemini 系); 实测(2026-08-14)部分中转模型
        (sensenova/agnes)只返回 url 直链——b64_json 为空时 url 直下兜底,
        下载超限/失败报错。空响应/双缺失 → 错误文本, 绝不返回空列表让
        调用方误报成功(§6.5)。"""
        items = (data or {}).get("data") or []
        if not items:
            return None, "[Error: image_gen 空响应 (data 为空数组/缺失)]"
        images = []
        for it in items:
            it = it or {}
            b64 = it.get("b64_json") or ""
            if b64:
                try:
                    images.append(base64.b64decode(b64))
                    continue
                except Exception:
                    return None, "[Error: image_gen b64 解码失败]"
            url = it.get("url") or ""
            if not url:
                return None, "[Error: image_gen 响应缺少 b64_json 与 url 字段]"
            raw, err = self._download(url)
            if err:
                return None, err
            images.append(raw)
        return images, None

    def _download(self, url):
        """url 直下兜底(部分中转只回直链): 流式读取, 超 20MiB 中止。
        公网直链无需鉴权头。返回 (bytes|None, err_text)。"""
        try:
            resp = self.http.get(url, stream=True, timeout=(self.connect_timeout, self.read_timeout),
                                 proxies=self.proxies, verify=self.verify)
        except (requests.Timeout, requests.ConnectionError, requests.exceptions.ChunkedEncodingError) as e:
            return None, f"[Error: image_gen 图片直链下载失败 {type(e).__name__}: {e}]"
        if resp.status_code >= 400:
            resp.close()
            return None, f"[Error: image_gen 图片直链下载失败 HTTP {resp.status_code}]"
        chunks = []
        total = 0
        try:
            for chunk in resp.iter_content(chunk_size=1 << 16):
                total += len(chunk)
                if total > _IMAGE_GEN_MAX_BYTES:
                    return None, "[Error: image_gen 图片直链超过 20MiB 交付上限]"
                chunks.append(chunk)
        except (requests.Timeout, requests.ConnectionError, requests.exceptions.ChunkedEncodingError) as e:
            return None, f"[Error: image_gen 图片直链下载中断 {type(e).__name__}: {e}]"
        finally:
            resp.close()
        if total == 0:
            return None, "[Error: image_gen 图片直链下载为空]"
        return b"".join(chunks), None

    def _validate_edit_images(self, images, profile=None):
        """把调用方给的参考图规整成 requests 的 multipart files 列表。

        images 元素: (filename, bytes, mime) —— **由工具层读盘并做路径安全/大小校验**，
        客户端只管发送(保持 llmcore 不依赖 cwd/文件系统语义, 也便于单测)。
        参考图上限真值在**档案**(limits.max_refs)；未声明上限时回退类常量。
        返回 (list[(name, raw, mime)], err_text)。"""
        if not images:
            return [], "[Error: image_gen 改图需要参考图(image 参数)]"
        max_refs = profile.max_refs if (profile and profile.max_refs) else self.MAX_EDIT_IMAGES
        if len(images) > max_refs:
            # 上限真值在档案（逐通道不同：JSON 形态实测 1..5，multipart 只读单值字段）
            src = (profile.source if profile else 'default')
            return [], (f"[Error: image_gen 改图当前仅支持 {max_refs} 张参考图"
                        f"(通道档案 limits.max_refs, 来源={src}；超过不静默只取第一张), 收到 {len(images)} 张——"
                        f"请改传 ≤{max_refs} 张，或分多次调用")
        out = []
        for idx, item in enumerate(images, 1):
            try:
                name, raw, mime = item[0], item[1], item[2]
            except (TypeError, IndexError):
                return [], f"[Error: image_gen 第 {idx} 张参考图格式非法]"
            if not raw:
                return [], f"[Error: image_gen 第 {idx} 张参考图为空: {name}]"
            if len(raw) > self.MAX_EDIT_IMAGE_BYTES:
                return [], (f"[Error: image_gen 第 {idx} 张参考图 {len(raw)} bytes 超过 "
                            f"{self.MAX_EDIT_IMAGE_BYTES // (1024 * 1024)}MiB 上限, 请先缩小]")
            out.append((name or f"ref{idx}.png", raw, mime or 'image/png'))
        return out, None

    def generate(self, prompt, size=None, quality=None, n=1, output_format=None, model=None, images=None):
        """统一入口。operation 由是否带参考图决定: 带图=改图(edit)、不带=文生图(generate)。

        **能力 gate**: 调用档案未声明的 operation 一律 fail-closed——因为存在"网关照文档
        收下参数但静默丢弃"的通道(实测 agnes 的 extra_body.image), 宁可知情失败, 不可假装成功。
        **语义上限 gate**: n 超档张数上限也不静默缩水(张数属用户可见契约)。
        返回 (images|None, err_text); 本次省略的装饰参数在 self.last_notices。"""
        images = list(images or [])
        model_eff = str(model or self.model)
        profile = self.profile_for(model_eff)
        operation = 'edit' if images else 'generate'
        self.last_notices = []
        if not profile.supports(operation):
            want = '改图(image.edit)' if operation == 'edit' else '文生图(image.generate)'
            have = ','.join(sorted(profile.ops)) or '无'
            return None, (f"[Error: image_gen 当前配置({model_eff} @ {self.api_base}) 未声明{want}能力"
                          f"(已声明: {have}, protocol={profile.api}, 档案来源={profile.source})——不要重试本工具, "
                          f"请如实告知用户该通道做不到, 或换用已声明该能力的配置]")
        n_int = max(1, int(n or 1))
        if n_int > profile.max_n:
            return None, (f"[Error: image_gen 通道({model_eff}) 单次最多 {profile.max_n} 张, 本次请求 {n_int} 张——"
                          f"张数是语义参数(用户可见契约), 不静默缩水; 请改为 ≤{profile.max_n} 张或换用支持更多的通道")
        if operation == 'edit':
            valid, ferr = self._validate_edit_images(images, profile)
            if ferr:
                return None, ferr
            if profile.api == self.PROTOCOL_EDITS:
                files = [("image", (name, raw, mime)) for name, raw, mime in valid]
                return self._generate_sync(prompt, size=size, quality=quality, n=n_int,
                                           output_format=output_format, model=model_eff, files=files,
                                           operation='edit', profile=profile)
            # images_edits_json: 图片以 images[{image_url: Data-URL}] 放进 JSON 体
            override, notices, perr = self._build_payload(
                profile, prompt, size=size, quality=quality, n=n_int,
                output_format=output_format, model=model_eff, stream=False)
            if perr:
                return None, perr
            self.last_notices = notices
            override['images'] = [{"image_url": _image_gen_data_url(raw, mime)} for _, raw, mime in valid]
            return self._generate_sync(prompt, size=size, quality=quality, n=n_int,
                                       output_format=output_format, model=model_eff,
                                       payload_override=override, operation='edit', profile=profile)
        if self.stream and n_int <= 1 and profile.wire_key('stream') is not None:
            # 流式仅档案声明支持 stream 的模型(gpt-image 系): dall-e/agnes 均未声明
            frame, err = self.generate_stream(prompt, size=size, quality=quality, n=n_int,
                                              output_format=output_format, model=model_eff, profile=profile)
            if frame is not None:
                return [frame], None
            print(f"[ImageGen] 流式路径失败({err}), 降级重试同步路径一次")
            images_out, sync_err = self._generate_sync(prompt, size=size, quality=quality, n=n_int,
                                                       output_format=output_format, model=model_eff,
                                                       profile=profile)
            if sync_err:
                return None, sync_err
            return images_out, None
        return self._generate_sync(prompt, size=size, quality=quality, n=n_int,
                                   output_format=output_format, model=model_eff, profile=profile)

    def _generate_sync(self, prompt, size=None, quality=None, n=1, output_format=None, model=None,
                       files=None, operation='generate', payload_override=None, profile=None):
        """同步路径: POST {apibase}/images/generations(或 /images/edits) → b64_json/url → bytes 列表。"""
        profile = profile or self.profile_for(model)
        if payload_override is not None:
            payload = payload_override
        else:
            payload, notices, perr = self._build_payload(profile, prompt, size=size, quality=quality, n=n,
                                                         output_format=output_format, model=model, stream=False)
            if perr:
                return None, perr
            self.last_notices = notices
        data, err = self._post(payload, stream=False, files=files, operation=operation, api=profile.api)
        if err:
            return None, err
        return self._extract_images(data)

    def generate_stream(self, prompt, size=None, quality=None, n=1, output_format=None, model=None, profile=None):
        """流式路径(仅档案声明支持 stream 的模型): stream:true + partial_images:0-3 SSE
        → 取最终帧。失败(SSE 解析失败/超时/无最终帧/收到非流式 JSON)由
        generate() 降级同步路径一次。返回 (final_frame_bytes|None, err_text)。"""
        profile = profile or self.profile_for(model)
        payload, notices, perr = self._build_payload(profile, prompt, size=size, quality=quality, n=n,
                                                     output_format=output_format, model=model, stream=True)
        if perr:
            return None, perr
        self.last_notices = notices
        wire_pi = profile.wire_key('partial_images')
        if wire_pi:
            payload[wire_pi] = 0  # 0=只要最终帧; 1-3=含渐进帧(不落盘)
        resp, err = self._post(payload, stream=True, api=profile.api)
        if err:
            return None, err
        ctype = (resp.headers or {}).get("content-type", "") or ""
        if "event-stream" not in ctype and "json" in ctype:
            # 中转网关把 SSE 折叠回普通 JSON(方案 §5): 直接按同步响应解析,
            # 避免二次请求重复计费。
            try:
                data = resp.json()
            except ValueError:
                return None, "[Error: image_gen 流式响应非 SSE 亦非 JSON]"
            finally:
                resp.close()
            images, err2 = self._extract_images(data)
            if err2:
                return None, err2
            return images[0], None
        final_frame = None
        try:
            for line in resp.iter_lines(decode_unicode=True):
                if not line:
                    continue
                line = line.strip()
                if not line.startswith("data:"):
                    continue
                data_str = line[len("data:"):].strip()
                if data_str == "[DONE]":
                    break
                try:
                    evt = json.loads(data_str)
                except ValueError:
                    continue  # 兼容注释/空 data 行
                if not isinstance(evt, dict):
                    continue
                if evt.get("error"):
                    em = evt["error"]
                    msg = em.get("message") if isinstance(em, dict) else str(em)
                    return None, f"[Error: image_gen 流式错误: {msg}]"
                d0 = (evt.get("data") or [{}])[0] or {}
                if isinstance(d0, dict) and d0.get("b64_json"):
                    final_frame = d0["b64_json"]  # 最终帧: 非最终帧无 b64_json
        except (requests.Timeout, requests.ConnectionError, requests.exceptions.ChunkedEncodingError, ValueError) as e:
            return None, f"[Error: image_gen 流式中断 {type(e).__name__}: {e}]"
        finally:
            resp.close()
        if not final_frame:
            return None, "[Error: image_gen 流式响应无最终帧]"
        try:
            return base64.b64decode(final_frame), None
        except Exception:
            return None, "[Error: image_gen b64 解码失败]"


class OpenAIImageGenClient(BaseImageGenClient):
    """OpenAI images/generations 兼容协议实现(直连/托管通用)。
    v1 唯一实现; 未来协议(fal/sdwebui/comfyui)加子类, resolve_image_gen 分派。"""


_IMAGE_GEN_SUPPORTED = ('openai', 'oai')


def _edit_config_name(name):
    """编辑通道配置名约定(确定性规则, 不猜也不留歧义):
    去掉结尾 `_gen` 再拼 `_edit`——`image_gen`→`image_edit`、`image`→`image_edit`、`foo`→`foo_edit`。
    好处: "免费文生图"与"付费改图"可以分开配置(与主流网关按 operation 路由到不同上游一致)。"""
    base = name[:-4] if str(name).endswith('_gen') else str(name)
    return f'{base}_edit'


def render_image_gen_capabilities(lang='zh', cfg_name='image_gen'):
    """把**当前配置通道**的能力渲染成文本（工具 schema 的能力段落，单一真值 = 档案）。

    存在的理由：能力知识写死在自然语言里必然与代码漂移——2026-09-14 实证：同一个工具
    描述里同时写着"当前路由上不存在参考图/改图能力"和"传 image 就是改图"。这里同一份
    档案既构造请求又渲染描述，不可能再自相矛盾。"""
    zh = str(lang).lower() != 'en'
    try:
        keys = reload_mykeys()[0] or {}
    except Exception:
        keys = {}
    if not keys.get(cfg_name):
        return ('生图通道未配置（image_gen）：调用本工具会立刻返回未配置错误——不要重试，如实告知用户生图未启用。'
                if zh else
                'Image channel not configured (image_gen): calls fail immediately. Do not retry; tell the user.')
    lines = []
    for name, label in ((cfg_name, '生图通道' if zh else 'generate channel'),
                        (_edit_config_name(cfg_name), '改图通道' if zh else 'edit channel')):
        sub = keys.get(name)
        if not sub:
            continue
        model = str(sub.get('model', ''))
        try:
            prof = resolve_image_profile(model, sub)
        except ValueError as e:
            lines.append(f"[{label}] 配置非法: {e}" if zh else f"[{label}] invalid config: {e}")
            continue
        host = str(sub.get('apibase', '')).split('//')[-1].split('/')[0]
        lines.append(f"[{label}] {prof.describe(model, host, zh)}")
    if not keys.get(_edit_config_name(cfg_name)):
        lines.append('改图通道未单独配置（image_edit）：改图能力以上述生图通道的档案声明为准。' if zh else
                     'No separate edit channel (image_edit): edit capability follows the generate channel profile above.')
    lines.append('未声明支持的能力一律 fail-closed（如实告知用户做不到，不要重试）；语义参数（size/n）不静默缩水。'
                 if zh else
                 'Unsupported capabilities fail closed (tell the user, do not retry); semantic params (size/n) are never silently shrunk.')
    return '\n'.join(lines)


def inject_image_gen_capabilities(schema, text, tool_name='image_gen'):
    """把能力文本注入指定工具的占位符 `{{IMAGE_GEN_CAPABILITIES}}`（其它工具不动）。"""
    token = '{{IMAGE_GEN_CAPABILITIES}}'
    out = []
    for item in schema or []:
        fn = item.get('function') if isinstance(item, dict) else None
        if isinstance(fn, dict) and fn.get('name') == tool_name and token in str(fn.get('description', '')):
            item = {**item, 'function': {**fn, 'description': fn['description'].replace(token, text)}}
        out.append(item)
    return out


def resolve_image_gen(name='image_gen', operation='generate'):
    """命名分派工厂(仿 resolve_session)。读 mykeys[name](经 reload_mykeys());
    未配置抛 ValueError —— 由 do_image_gen 捕获并返回错误文本, 绝不裸抛穿透
    dispatch(agent_loop 只捕 StopIteration)。

    operation='edit' 时的配置解析顺序(2026-09-13, 见 image-capability/DESIGN §4.1):
      ① mykeys['<name>_edit'] (如 image_edit) 存在 → 用它——把"便宜/免费的文生图
         通道"与"付费的改图通道"分开配置(主流网关按 operation 路由到不同上游);
      ② 否则用 mykeys[name], 但它的 operations 必须含 'edit' → 否则由客户端
         fail-closed 报错(不用静默丢弃参数的通道假装成功)。"""
    keys = reload_mykeys()[0]
    cfg = None
    if str(operation).lower() == 'edit':
        cfg = keys.get(_edit_config_name(name)) or keys.get(name)
    else:
        cfg = keys.get(name)
    if not cfg:
        raise ValueError(f"Config '{name}' not in mykey")
    kind = str(cfg.get('name', '') or '').strip().lower()
    if not kind or kind in _IMAGE_GEN_SUPPORTED:
        return OpenAIImageGenClient(cfg)
    raise ValueError(f"image_gen: 不支持的客户端类型 {kind!r} (v1: openai/oai)")
