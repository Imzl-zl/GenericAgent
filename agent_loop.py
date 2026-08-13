import json, re, os, base64
from dataclasses import dataclass
from typing import Any, Optional
try: from plugins.hooks import trigger as _hook
except ImportError: _hook = lambda *a, **k: None
@dataclass
class StepOutcome:
    data: Any
    next_prompt: Optional[str] = None
    should_exit: bool = False
def try_call_generator(func, *args, **kwargs):
    ret = func(*args, **kwargs)
    if hasattr(ret, '__iter__') and not isinstance(ret, (str, bytes, dict, list)): ret = yield from ret
    return ret

class BaseHandler:
    def turn_end_callback(self, response, tool_calls, tool_results, turn, next_prompt, exit_reason): return next_prompt
    def dispatch(self, tool_name, args, response, index=0, tool_num=1):
        method_name = f"do_{tool_name}"
        if hasattr(self, method_name):
            args['_index'] = index; args['_tool_num'] = tool_num
            _hook('tool_before', locals())
            ret = yield from try_call_generator(getattr(self, method_name), args, response)
            _hook('tool_after', locals())
            return ret
        elif tool_name == 'bad_json': return StepOutcome(None, next_prompt=args.get('msg', 'bad_json'), should_exit=False)
        else:
            yield f"未知工具: {tool_name}\n"
            return StepOutcome(None, next_prompt=f"未知工具 {tool_name}", should_exit=False)

def json_default(o): return list(o) if isinstance(o, set) else str(o)
def exhaust(g):
    try: 
        while True: next(g)
    except StopIteration as e: return e.value

def get_pretty_json(data):
    if isinstance(data, dict) and "script" in data:
        data = data.copy(); data["script"] = data["script"].replace("; ", ";\n  ")
    return json.dumps(data, indent=2, ensure_ascii=False).replace('\\n', '\n')

# 2026-08-13: 附件图片直传多模态模型(生产实证: agnes-2.5-flash 收不到
# 图片内容只能靠 code_run 瞎折腾, 反复生成 shell 脚本 SyntaxError 直到
# 任务超时)。把 prompt 里引用的 attachments/*.图片 base64 编码为
# image_url 块注入第一轮 user content——NativeOAISession 通道原生支持
# (llmcore _msgs_claude2oai 保留 image_url), 旧 ToolClient 文本协议降级
# 为字符串(不崩但看不到图)。限制: 单图 <=3MB, 最多 3 张, 防 context 撑爆。
_ATTACH_IMG_RE = re.compile(r'(attachments/[^\s\],]+?\.(?:jpg|jpeg|png|gif|webp|bmp))', re.I)
_ATTACH_IMG_MAX_BYTES = 3 * 1024 * 1024
_ATTACH_IMG_MAX_COUNT = 3

# 附件根: 优先 GA 的 temp 目录(平台/桌面统一, 进程 cwd 可能是 /ga/legacy
# 而不是 temp, 相对路径会解析错位——2026-08-13 生产实证); 回退 cwd。
_ATTACH_BASE = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'temp')
_IMG_MIME = {'jpg': 'image/jpeg', 'jpeg': 'image/jpeg', 'png': 'image/png',
             'gif': 'image/gif', 'webp': 'image/webp', 'bmp': 'image/bmp'}


def _resolve_attach_path(rel):
    for base in (_ATTACH_BASE, os.getcwd()):
        p = os.path.join(base, rel)
        if os.path.isfile(p):
            return p
    return None


def _image_block_from_file(full, rel):
    """读图片文件 → image_url block。PIL 可用时降采样(最长边 1568px,
    对齐主流视觉 token 成本控制——Claude Code 同款策略), 失败/无 PIL
    时原样 base64。返回 (block, injected_bytes) 或 (None, 0)。"""
    ext = os.path.splitext(rel)[1].lstrip('.').lower()
    mime = _IMG_MIME.get(ext, 'image/jpeg')
    try:
        with open(full, 'rb') as f:
            raw = f.read()
    except OSError:
        return None, 0
    data = raw
    try:
        from PIL import Image
        import io
        img = Image.open(io.BytesIO(raw))
        img.load()
        longest = max(img.size)
        if longest > 1568:
            ratio = 1568.0 / longest
            img = img.resize((max(1, int(img.width * ratio)), max(1, int(img.height * ratio))),
                             Image.LANCZOS)
        if img.mode in ('RGBA', 'P', 'LA'):
            img = img.convert('RGB')
        buf = io.BytesIO()
        img.save(buf, format='JPEG', quality=85)
        data = buf.getvalue()
        mime = 'image/jpeg'
    except Exception:
        pass  # 无 PIL/解码失败: 原样透传, 由上游决定
    b64 = base64.b64encode(data).decode('ascii')
    return {"type": "image_url", "image_url": {"url": f"data:{mime};base64,{b64}"}}, len(data)


def media_content_blocks(user_text, image_paths=None):
    """结构化媒体注入(2026-08-13 多模态链路定案):

    - image_paths: 显式媒体路径列表(相对 GA temp 或绝对路径)——主路径,
      来自 GA 原生 put_task(images=...)(worker 经 TaskEnvelope.media 契约
      传递, 平台/桌面/CLI 统一)。
    - 无显式路径时回退: 从 user_text 正则提取 attachments/ 图片引用(兼容
      旧调用方/纯文本路径约定, 兜底层)。
    - 返回 list[blocks](text + image_url) 或原字符串(无图时); 非图片
      扩展名/超限文件跳过。扩展音频/视频/PDF 时在此加分支, 链路不动。
    """
    if not isinstance(user_text, str):
        return user_text
    refs = list(image_paths or [])
    if not refs:
        refs, seen = [], set()
        for m in _ATTACH_IMG_RE.finditer(user_text):
            rel = m.group(1)
            if rel in seen:
                continue
            seen.add(rel)
            refs.append(rel)
    picked = []
    for rel in refs:
        if len(picked) >= _ATTACH_IMG_MAX_COUNT:
            break
        full = _resolve_attach_path(rel)
        if full is None:
            continue
        try:
            if os.path.getsize(full) > _ATTACH_IMG_MAX_BYTES:
                continue
        except OSError:
            continue
        if os.path.splitext(rel)[1].lstrip('.').lower() not in _IMG_MIME:
            continue
        picked.append((rel, full))
    if not picked:
        return user_text
    blocks = [{"type": "text", "text": user_text}]
    injected = 0
    for rel, full in picked:
        block, n = _image_block_from_file(full, rel)
        if block is not None:
            blocks.append(block)
            injected += n
    if len(blocks) == 1:
        return user_text
    print(f"[Attach] injected {len(blocks) - 1} image block(s) ({injected} bytes) into first turn")
    return blocks


# 兼容别名: 旧调用方(2026-08-13 补丁版)仍可用; 新代码走 media_content_blocks。
_inject_attachment_images = media_content_blocks


def agent_runner_loop(client, system_prompt, user_input, handler, tools_schema, 
                      max_turns=40, verbose=True, initial_user_content=None, yield_info=False):
    user_content = initial_user_content if initial_user_content is not None else _inject_attachment_images(user_input)
    messages = [
        {"role": "system", "content": system_prompt},
        {"role": "user", "content": user_content}
    ]
    turn = 0;  handler.max_turns = max_turns
    _hook('agent_before', locals())
    while turn < handler.max_turns:
        turn += 1; turnstr = f'LLM Running (Turn {turn}) ...'
        if handler.parent.task_dir: turnstr = f'Turn {turn} ...'
        if verbose: turnstr = f'**{turnstr}**'
        if yield_info: yield {'turn': turn}
        # 输出分层(架构): verbose=True 输出完整过程转录(TUI/CLI/桌面等
        # 展示思考过程的前端); verbose=False 只输出用户可见回复文本
        # (租户 worker 交付 + 根项目 IM 前端, 用户不应看到轮次标记/工具
        # 调用等内部过程)。事件 dict({'turn': N})两种模式都发(前端轮次
        # 协调信号, 非用户文本)。
        if verbose:
            yield f"\n{turnstr}\n\n"
        if turn%10 == 0: client.last_tools = ''  # 每10轮重置一次工具描述
        _hook('turn_before', locals())
        _hook('llm_before', locals())
        response_gen = client.chat(messages=messages, tools=tools_schema)
        if verbose:
            response = yield from response_gen
            yield '\n\n'
        else:
            response = exhaust(response_gen)
            cleaned = _clean_content(response.content)
            if cleaned: yield cleaned + '\n'
        _hook('llm_after', locals())

        if not response.tool_calls: tool_calls = [{'tool_name': 'no_tool', 'args': {}}]
        else: tool_calls = [{'tool_name': tc.function.name, 'args': json.loads(tc.function.arguments), 'id': tc.id}
                          for tc in response.tool_calls]
       
        tool_results = []; next_prompts = set(); exit_reason = {}
        for ii, tc in enumerate(tool_calls):
            tool_name, args, tid = tc['tool_name'], tc['args'], tc.get('id', '')
            if tool_name == 'no_tool': pass
            elif verbose:
                # 工具调用痕迹是过程转录的一部分: 仅 verbose 输出(IM 交付
                # 不暴露内部工具调用)。
                yield f"🛠️ Tool: `{tool_name}`  📥 args:\n````text\n{get_pretty_json(args)}\n````\n"
            elif yield_info:
                # 工具活动事件(非用户文本, 供 worker 心跳保活/前端协调):
                # 非 verbose 的工具执行可能长达数分钟, 若无任何事件,
                # worker 的推进窗口(150s)到期后心跳停发, 长工具轮会被
                # idle reaper 误收割。verbose 模式已有完整工具文本流,
                # 无需事件。
                yield {'tool': tool_name}
            handler.current_turn = turn
            gen = handler.dispatch(tool_name, args, response, index=ii, tool_num=len(tool_calls))
            try:
                v = next(gen)
                def proxy(): yield v; return (yield from gen)
                if verbose: yield '`````\n'
                outcome = (yield from proxy()) if verbose else exhaust(proxy())
                if verbose: yield '`````\n'
            except StopIteration as e: outcome = e.value
            
            if outcome.should_exit: 
                exit_reason = {'result': 'EXITED', 'data': outcome.data}; break
            if not outcome.next_prompt: 
                exit_reason = {'result': 'CURRENT_TASK_DONE', 'data': outcome.data}; break
            if outcome.next_prompt.startswith('未知工具'): client.last_tools = ''
            if outcome.data is not None and tool_name != 'no_tool': 
                datastr = json.dumps(outcome.data, ensure_ascii=False, default=json_default) if type(outcome.data) in [dict, list] else str(outcome.data) 
                tool_results.append({'tool_use_id': tid, 'content': datastr})
            next_prompts.add(outcome.next_prompt)
        if len(next_prompts) == 0 or exit_reason:
            if len(handler._done_hooks) == 0 or exit_reason.get('result', '') == 'EXITED': break
            next_prompts.add(handler._done_hooks.pop(0))
        next_prompt = handler.turn_end_callback(response, tool_calls, tool_results, turn, '\n'.join(next_prompts), exit_reason)
        _hook('turn_after', locals())
        messages = [{"role": "user", "content": next_prompt, "tool_results": tool_results}]   # just new message, history is kept in *Session
    if exit_reason: handler.turn_end_callback(response, tool_calls, tool_results, turn, '', exit_reason)
    _hook('agent_after', locals())
    return exit_reason or {'result': 'MAX_TURNS_EXCEEDED'}

def _clean_content(text):
    if not text: return ''
    def _shrink_code(m):
        lines = m.group(0).split('\n')
        lang = lines[0].replace('```','').strip()
        body = [l for l in lines[1:-1] if l.strip()]
        if len(body) <= 6: return m.group(0)
        preview = '\n'.join(body[:5])
        return f'```{lang}\n{preview}\n  ... ({len(body)} lines)\n```'
    text = re.sub(r'```[\s\S]*?```', _shrink_code, text)
    for p in [r'<file_content>[\s\S]*?</file_content>', r'<tool_(?:use|call)>[\s\S]*?</tool_(?:use|call)>', r'(\r?\n){3,}']:
        text = re.sub(p, '\n\n' if '\\n' in p else '', text)
    # 工作记忆块(<summary>)是系统提示词要求的模型输出, 供 GA 记忆层消费,
    # 不属于用户可见回复: 非 verbose 分支(用户可见输出)必须剥离。
    # 语义边界: 假设模型回复正文不会出现字面 <summary> 标签(该标签是系统
    # 提示词保留给工作记忆块的格式), 出现即视为内部块剥离。
    text = re.sub(r'<summary>[\s\S]*?</summary>', '', text, flags=re.IGNORECASE)
    return text.strip()

