import os, sys, threading, queue, time, json, re, random, locale, glob
os.environ.setdefault('GA_LANG', 'zh' if any(k in (locale.getlocale()[0] or '').lower() for k in ('zh', 'chinese')) else 'en')
if sys.stdout is None: sys.stdout = open(os.devnull, "w")
elif hasattr(sys.stdout, 'reconfigure'): sys.stdout.reconfigure(errors='replace')
if sys.stderr is None: sys.stderr = open(os.devnull, "w")
elif hasattr(sys.stderr, 'reconfigure'): sys.stderr.reconfigure(errors='replace')
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))

from llmcore import reload_mykeys, ToolClient, MixinSession, NativeToolClient, NativeClaudeSession, NativeOAISession, resolve_client
from agent_loop import agent_runner_loop, media_content_blocks
try:
    from plugins.hooks import discover_and_load; discover_and_load()
except Exception: pass
from ga import GenericAgentHandler, smart_format, get_global_memory, format_error, consume_file

script_dir = os.path.dirname(os.path.abspath(__file__))
BANNED_TOOLS = (['ask_user', 'start_long_term_update'] if '--no-user-tools' in sys.argv else [])
def load_tool_schema(suffix=''):
    global TOOLS_SCHEMA
    TS = open(os.path.join(script_dir, f'assets/tools_schema{suffix}.json'), 'r', encoding='utf-8').read()
    TOOLS_SCHEMA = json.loads(TS if os.name == 'nt' else TS.replace('powershell', 'bash'))
    TOOLS_SCHEMA = [t for t in TOOLS_SCHEMA if t.get('function', {}).get('name') not in BANNED_TOOLS]
load_tool_schema()

lang_suffix = '_en' if os.environ.get('GA_LANG', '') == 'en' else ''
mem_dir = os.path.join(script_dir, 'memory')
if not os.path.exists(mem_dir): os.makedirs(mem_dir)
mem_txt = os.path.join(mem_dir, 'global_mem.txt')
if not os.path.exists(mem_txt): open(mem_txt, 'w', encoding='utf-8').write('# [Global Memory - L2]\n')
mem_insight = os.path.join(mem_dir, 'global_mem_insight.txt')
if not os.path.exists(mem_insight):
    t = os.path.join(script_dir, f'assets/global_mem_insight_template{lang_suffix}.txt')
    open(mem_insight, 'w', encoding='utf-8').write(open(t, encoding='utf-8').read() if os.path.exists(t) else '')

def get_system_prompt():
    with open(os.path.join(script_dir, f'assets/sys_prompt{lang_suffix}.txt'), 'r', encoding='utf-8') as f: prompt = f.read()
    prompt += f"\nToday: {time.strftime('%Y-%m-%d %a')}\n"
    prompt += get_global_memory()
    return prompt

# SDK:
# agent = GenericAgent(); threading.Thread(target=agent.run, daemon=True).start()
# output1_queue = agent.put_task(prompt1)
# output2_queue = agent.put_task(prompt2)
class GenericAgent:
    def __init__(self):
        os.makedirs(os.path.join(script_dir, 'temp'), exist_ok=True)
        self.lock = threading.Lock()
        self.task_dir = None
        self.history = []; self.handler = None; 
        self.task_queue = queue.Queue() 
        self.is_running = False; self.stop_sig = False; self.llm_no = 0;  
        self.inc_out = False; self.verbose = True
        self.peer_hint = True
        self.force_non_stream = False
        self._shutdown = False
        self._runner_thread = None
        logid = f'{(time.time_ns() + random.randrange(1_000_000)) % 1_000_000:06d}'
        self.log_path = os.path.join(script_dir, f'temp/model_responses/model_responses_{logid}.txt')
        self.llmclient = None
        self.load_llm_sessions()
        self.extra_sys_prompts = []
        self.intervene = self.extrakeyinfo = None

    def load_llm_sessions(self):
        mykeys, changed = reload_mykeys()
        if not changed and hasattr(self, 'llmclients'): return
        try: oldhistory, oldname = self.llmclient.backend.history, self.llmclient.backend.name
        except: oldhistory = oldname = None
        llm_sessions = []
        for k, cfg in mykeys.items():
            if not any(x in k for x in ['api', 'config', 'cookie']): continue
            try:
                if 'mixin' in k: llm_sessions += [{'mixin_cfg': cfg}]
                elif c := resolve_client(k): llm_sessions += [c]
                # 2026-08-14 审查 S-3: 配置名不含 native/claude/oai 标记时
                # resolve_client 静默返回 None——用户配置名拼错时 /llms 缺项
                # 且无提示, 必须显式警告。
                else: print(f'[WARN] config {k!r} matches no session type (name must contain native/claude/oai), skipped')
            except Exception as e: print(f'[WARN] failed to init config {k!r}: {e}')
        for i, s in enumerate(llm_sessions):
            if isinstance(s, dict) and 'mixin_cfg' in s:
                try:
                    mixin = MixinSession(llm_sessions, s['mixin_cfg'])
                    if isinstance(mixin._sessions[0], (NativeClaudeSession, NativeOAISession)): llm_sessions[i] = NativeToolClient(mixin)
                    else: llm_sessions[i] = ToolClient(mixin)
                except Exception as e: print(f'\n\n\n[ERROR] Failed to init MixinSession with cfg {s["mixin_cfg"]}: {e}!!!\n\n')
        self.llmclients = llm_sessions
        if not self.llmclients: return
        names = [c.backend.name if not isinstance(c, dict) else f'BADMIXIN_{i}' for i, c in enumerate(self.llmclients)]
        if oldname in names: self.llm_no = names.index(oldname)
        self.llmclient = self.llmclients[self.llm_no%len(self.llmclients)]
        if oldhistory: self.llmclient.backend.history = oldhistory
    
    def next_llm(self, n=-1):
        self.load_llm_sessions()
        if not self.llmclients: return
        self.llm_no = ((self.llm_no + 1) if n < 0 else n) % len(self.llmclients)
        lastc = self.llmclient
        self.llmclient = self.llmclients[self.llm_no]
        try: self.llmclient.backend.history = lastc.backend.history
        except: raise Exception('[ERROR] BAD Mixin config: Check your mykey.py')
        self.llmclient.last_tools = ''
        load_tool_schema()
    def list_llms(self): 
        self.load_llm_sessions()
        return [(i, self.get_llm_name(b), i == self.llm_no) for i, b in enumerate(self.llmclients)]
    def get_llm_name(self, b=None, model=False):
        b = self.llmclient if b is None else b
        if isinstance(b, dict): return 'BADCONFIG_MIXIN'
        if model: return b.backend.model.lower()
        return f"{type(b.backend).__name__.replace('Session', '')}/{b.backend.name}"
    def get_ctx_multiplier(self): return getattr(self.llmclient.backend, 'maxlen_multiplier', 1.0)

    def abort(self):
        if not self.is_running: return
        print('Abort current task...')
        self.stop_sig = True
        for sess in getattr(self.llmclient.backend, '_sessions', [self.llmclient.backend]):
            try: sess.active_response.close()
            except Exception: pass
        if self.handler is not None: self.handler.code_stop_signal.append(1)

    def shutdown(self, join_timeout=1.0):
        with self.lock:
            if self._shutdown:
                runner = self._runner_thread
            else:
                self._shutdown = True
                runner = self._runner_thread
        self.abort()
        try: self.task_queue.put("STOP")
        except Exception: pass
        if runner is not None and runner.is_alive() and runner is not threading.current_thread():
            runner.join(timeout=join_timeout)

    def put_task(self, query, source="user", images=None):
        if self._shutdown: raise RuntimeError('GenericAgent is shut down')
        display_queue = queue.Queue()
        self.task_queue.put({"query": query, "source": source, "images": images or [], "output": display_queue})
        return display_queue

    # i know it is dangerous, but raw_query is dangerous enough it doesn't enlarge
    def _handle_slash_cmd(self, raw_query, display_queue):
        if not raw_query.startswith('/'): return raw_query
        if _sm := re.match(r'/session\.(\w+)=(.*)', raw_query.strip()):
            k, v = _sm.group(1), _sm.group(2)
            vfile = os.path.join(script_dir, 'temp', v)
            if os.path.isfile(vfile): v = open(vfile, encoding='utf-8').read().strip()
            try: v = json.loads(v)  # cover number parsing
            except (json.JSONDecodeError, ValueError): pass
            setattr(self.llmclient.backend, k, v)
            display_queue.put({'done': smart_format(f"✅ session.{k} = {repr(v)}", max_str_len=500), 'source': 'system'})
            return None
        if raw_query.strip() == '/resume':
            return r'帮我看看最近有哪些会话可以恢复。读model_responses/目录，按修改时间取最近10个文件，从每个文件里找最后一个<history>...</history>块，用一句话总结每个会话在聊什么，列表给我选。注意读文件后要把字面的\n替换成真换行才能正确匹配。'
        return raw_query

    def run(self):
        self._runner_thread = threading.current_thread()
        while True:
            task = self.task_queue.get()
            if isinstance(task, str): break
            if self._shutdown:
                try: task["output"].put({"done": "", "error": "GenericAgent is shutting down", "source": task.get("source", "system")})
                except Exception: pass
                self.task_queue.task_done()
                continue
            raw_query, source, display_queue = task["query"], task["source"], task["output"]
            # 2026-08-13 多模态链路: put_task(images=...) 结构化媒体参数补完
            # (GA 原生设计意图, 此前为死参数)。run() 消费后经
            # media_content_blocks 把图片作为 content block 注入模型首轮。
            task_images = task.get("images") or []
            raw_query = self._handle_slash_cmd(raw_query, display_queue)
            if raw_query is None:
                self.task_queue.task_done(); continue
            self.is_running = True
            if len(raw_query) > 2000:
                task_file = os.path.join(script_dir, 'temp', f'user_prompt_{os.getpid()}_{time.time_ns()}.md')
                with open(task_file, 'w', encoding='utf-8') as f: f.write(raw_query)
                raw_query = f'Long user prompt saved to {task_file}. Read and execute.'
            rquery = smart_format(raw_query.replace('\n', ' '), max_str_len=200)
            self.history.append(f"[USER]: {rquery}")
            if self.llmclient is None:
                # 审查 F4: 无可用模型配置时不得让 runner 线程在任务级 try 外
                # 访问 None.backend 崩溃——调用方(CLI/前端)会永久等待终态。
                # 显式发送结构化错误终态, 保持任务语义完整。
                display_queue.put({'done': '', 'error': 'No LLM backend configured; check mykey.py', 'error_code': 'NO_LLM_CONFIG', 'source': source, 'turn': 0, 'outputs': []})
                self.task_queue.task_done()
                self.is_running = False
                continue
            sys_prompt = get_system_prompt() + '\n'.join(self.extra_sys_prompts) + getattr(self.llmclient.backend, 'extra_sys_prompt', '')
            if self.peer_hint: sys_prompt += f"\n[Peer] 用户提及其他会话/后台任务状态时: temp/model_responses/ (只找近期修改的文件尾部)\n"
            handler = GenericAgentHandler(self, self.history, os.path.join(script_dir, 'temp'))
            if getattr(self, 'no_print', False): handler.print = lambda *a, **k: None
            if self.handler and 'key_info' in self.handler.working: 
                ki = re.sub(r'\n\[SYSTEM\] 此为.*?工作记忆[。\n]*', '', self.handler.working['key_info'])  # 去旧
                handler.working['key_info'] = ki
                handler.working['passed_sessions'] = ps = self.handler.working.get('passed_sessions', 0) + 1
                if ps > 0: handler.working['key_info'] += f'\n[SYSTEM] 此为 {ps} 个对话前设置的key_info，若已在新任务，先更新或清除工作记忆。\n'
            self.handler = handler  # although new handler, the **full** history is in llmclient, so it is full history!
            self.llmclient.log_path = self.log_path
            if self.force_non_stream:
                self.llmclient.backend.stream = False
                self.llmclient.backend.read_timeout = max(self.llmclient.backend.read_timeout, 1200)
            gen = agent_runner_loop(self.llmclient, sys_prompt, raw_query, handler, TOOLS_SCHEMA, 
                                    max_turns=180, verbose=self.verbose, yield_info=True,
                                    initial_user_content=(
                                        media_content_blocks(raw_query, task_images) if task_images else None))
            try:
                full_resp = ""; last_pos = 0; curr_turn = 0; turn_resps = []
                runner_result = {}
                while True:
                    try:
                        chunk = next(gen)
                    except StopIteration as stop:
                        runner_result = stop.value or {}
                        break
                    if consume_file(self.task_dir, '_stop'): self.abort() 
                    if self.stop_sig: break
                    if isinstance(chunk, dict) and 'turn' in chunk:
                        # 轮次边界事件(输出分层架构的配套信号, 2026-08-12):
                        # ①先冲刷上一轮残留文本——保证 'next' 不跨轮次边界,
                        # 前端按 turn 事件切分消息时文本归属精确;
                        # ②再发 turn 事件(消费方协调轮次/心跳保活)。
                        # outputs 与 'next' 同构(turn_resps[-2:]), 兼容
                        # wechatapp 按 outputs[-2] 落定上一轮文本的消费方式。
                        if last_pos < len(full_resp):
                            display_queue.put({'next': full_resp[last_pos:] if self.inc_out else full_resp,
                                               'source': source, 'turn': curr_turn,
                                               'outputs': turn_resps[-2:]})
                            last_pos = len(full_resp)
                        curr_turn = chunk['turn']; turn_resps.append('')
                        display_queue.put({'turn': curr_turn, 'source': source,
                                           'outputs': turn_resps[-2:]})
                        continue
                    if isinstance(chunk, dict) and 'tool' in chunk:
                        # 工具活动事件(非用户文本): worker 心跳推进信号
                        # (长工具轮无文本输出时不至于被 idle reaper 误收割);
                        # 消费方只认 next/done 键, 此事件安全忽略。
                        display_queue.put({'tool': chunk['tool'], 'source': source,
                                           'turn': curr_turn})
                        continue
                    full_resp += chunk;  turn_resps[-1] += chunk
                    # 'LLM Running' 条件服务 verbose 路径: 轮次标记文本需立即
                    # 推给前端(TUI 思考提示); 非 verbose 无标记, 仅靠 >30 字符
                    # 阈值推进。
                    if len(full_resp) - last_pos > 30 or 'LLM Running' in chunk:
                        display_queue.put({'next': full_resp[last_pos:] if self.inc_out else full_resp, 
                                           'source': source, 'turn': curr_turn, 'outputs': turn_resps[-2:]})
                        last_pos = len(full_resp)
                if self.inc_out and last_pos < len(full_resp):
                    display_queue.put({'next': full_resp[last_pos:], 'source': source,
                                    'turn': curr_turn, 'outputs': turn_resps[-2:]})
                done_item = {'done': full_resp, 'source': source, 'turn': curr_turn, 'outputs': turn_resps.copy()}
                if runner_result.get('result') == 'MAX_TURNS_EXCEEDED':
                    done_item.update({
                        'error': '任务超出最大轮数仍未完成，已停止（可能是任务过于复杂或上游模型异常），请重试或拆分后再试',
                        'error_code': 'MAX_TURNS_EXCEEDED',
                    })
                elif isinstance(runner_result.get('data'), dict) and runner_result.get('data', {}).get('result') == 'LLM_FAILED':
                    # 审查 D5: LLM 传输/HTTP/解析故障连续发生(重试耗尽)后
                    # 结构化标记失败——Worker 据此映射 TASK_FAILED, 而不是把
                    # 故障文本当作成功结果提交。
                    # agent_loop 将 should_exit 的 outcome.data 包装为
                    # {'result':'EXITED','data':...}, LLM_FAILED 嵌套在 data 内;
                    # data 也可能是任意对象(如正常完成时 do_no_tool 的 response),
                    # 故必须先用 isinstance 判定再读取(勿改回顶层判定)。
                    # 2026-08-13: 用户可见文案不再透传"给模型的英文指令"
                    # (如 [System] Incomplete response...), 统一为友好中文。
                    _llm_fail = runner_result.get('data') or {}
                    done_item.update({
                        'error': '模型连续多次未返回有效内容（可能是上游模型异常或繁忙），请重试或稍后再试',
                        'error_code': 'LLM_FAILED',
                    })
                display_queue.put(done_item)
                self.history = handler.history_info
            except Exception as e:
                # B1: surface backend exceptions distinctly via 'error' key so the
                # Worker adapter maps them to TASK_FAILED instead of TASK_SUCCEEDED.
                # Keep 'done' = partial body for CLI backward compat; the error is
                # also printed to stdout below for interactive users.
                print(f"Backend Error: {format_error(e)}")
                display_queue.put({'done': full_resp, 'error': format_error(e), 'source': source, 'turn': curr_turn, 'outputs': turn_resps.copy()})
            finally:
                if self.stop_sig: print('User aborted the task.')
                self.is_running = self.stop_sig = False
                self.task_queue.task_done()
                if self.handler is not None: self.handler.code_stop_signal.append(1)

GeneraticAgent = GenericAgent

if __name__ == '__main__':
    import argparse
    from datetime import datetime
    parser = argparse.ArgumentParser()
    parser.add_argument('--task', metavar='IODIR', help='一次性任务模式，先看subagent.md')
    parser.add_argument('--func', metavar='PROMPT_FILE', help='纯函数模式：读prompt文件→结果写prompt.out.txt→退出')
    parser.add_argument('--reflect', metavar='SCRIPT', help='反射模式：加载监控脚本，check()触发时发任务')
    parser.add_argument('--input', help='prompt')
    parser.add_argument('--history', help='history json file')
    parser.add_argument('--llm_no', type=int, default=0)
    parser.add_argument('--verbose', action='store_true')
    parser.add_argument('--nobg', action='store_true')
    parser.add_argument('--nolog', action='store_true')
    parser.add_argument('--no-user-tools', action='store_true')
    args, _unknown = parser.parse_known_args()
    _extra_args = dict(zip([k.lstrip('-') for k in _unknown[::2]], _unknown[1::2])) if _unknown else {}

    if (args.func or args.task) and not args.nobg:
        import subprocess, platform
        cmd = [sys.executable, os.path.abspath(__file__)] + [a for a in sys.argv[1:]] + ['--nobg']
        if args.task:
            d = os.path.join(script_dir, f'temp/{args.task}'); os.makedirs(d, exist_ok=True)
            out = open(os.path.join(d, 'stdout.log'), 'w', encoding='utf-8')
            err = open(os.path.join(d, 'stderr.log'), 'w', encoding='utf-8')
        else: out, err = subprocess.DEVNULL, subprocess.DEVNULL
        p = subprocess.Popen(cmd, cwd=script_dir,
            creationflags=0x08000000 if platform.system() == 'Windows' else 0,
            stdout=out, stderr=err)
        print('PID:', p.pid); sys.exit(0)

    agent = GenericAgent()
    if args.nolog: agent.log_path = False
    agent.next_llm(args.llm_no)
    agent.verbose = args.verbose
    threading.Thread(target=agent.run, daemon=True).start()

    histfile = args.history
    if args.task:
        agent.task_dir = d = os.path.join(script_dir, f'temp/{args.task}'); nround = ''
        infile = os.path.join(d, 'input.txt'); outfile = f'{d}/output{nround}.txt'
        if args.input:
            os.makedirs(d, exist_ok=True)
            [os.remove(f) for f in glob.glob(os.path.join(d, 'output*.txt'))]
            with open(infile, 'w', encoding='utf-8') as f: f.write(args.input)
        histfile = histfile or os.path.join(d, '_history.json')
    elif args.func:
        infile = args.func; outfile = os.path.splitext(args.func)[0] + '.out.txt'

    if histfile and os.path.isfile(histfile): agent.llmclient.backend.history = json.loads(open(histfile, encoding='utf-8').read())

    if args.func or args.task:
        agent.peer_hint = False
        with open(infile, encoding='utf-8') as f: raw = f.read()
        while True:
            dq = agent.put_task(raw, source='func' if args.func else 'task')
            while 'done' not in (item := dq.get(timeout=2200)):
                if 'next' in item:
                    with open(outfile, 'w', encoding='utf-8') as f: f.write(item.get('next', ''))
            with open(outfile, 'w', encoding='utf-8') as f: f.write(item['done'] + '\n\n[ROUND END]\n')
            if not args.task: break
            consume_file(d, '_stop')  # 已经成功停下来了，避免打断下次reply
            for _ in range(300):  # 等reply.txt，10分钟超时
                time.sleep(2)
                if (raw := consume_file(d, 'reply.txt')): break
            else: break
            nround = nround + 1 if isinstance(nround, int) else 1
            outfile = f'{d}/output{nround}.txt'
    elif args.reflect:
        agent.peer_hint = False
        import importlib.util
        spec = importlib.util.spec_from_file_location('reflect_script', args.reflect)
        mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
        if hasattr(mod, 'init'): mod.init(_extra_args)
        _mt = os.path.getmtime(args.reflect)
        print(f'[Reflect] loaded {args.reflect}' + (f' args={_extra_args}' if _extra_args else ''))
        while True:
            if os.path.getmtime(args.reflect) != _mt:
                try:
                    spec.loader.exec_module(mod); _mt = os.path.getmtime(args.reflect)
                    if hasattr(mod, 'init'): mod.init(_extra_args)
                    print('[Reflect] reloaded')
                except Exception as e: print(f'[Reflect] reload error: {e}')
            try: task = mod.check()
            except Exception as e: 
                print(f'[Reflect] check() error: {e}'); task = None
            if task and task == '/exit': break
            if task:
                print(f'[Reflect] triggered: {task[:80]}')
                dq = agent.put_task(task, source='reflect')
                try:
                    while 'done' not in (item := dq.get(timeout=2200)): pass
                    result = item['done']
                    print(result)
                except Exception as e:
                    if getattr(mod, 'ONCE', False): raise
                    print(f'[Reflect] drain error: {e}'); result = f'[ERROR] {e}'
                log_dir = os.path.join(script_dir, 'temp/reflect_logs'); os.makedirs(log_dir, exist_ok=True)
                script_name = os.path.splitext(os.path.basename(args.reflect))[0]
                open(os.path.join(log_dir, f'{script_name}_{datetime.now():%Y-%m-%d}.log'), 'a', encoding='utf-8').write(f'[{datetime.now():%m-%d %H:%M}]\n{result}\n\n')
                if (on_done := getattr(mod, 'on_done', None)):
                    try: on_done(result)
                    except Exception as e: print(f'[Reflect] on_done error: {e}')
                if getattr(mod, 'ONCE', False): print('[Reflect] ONCE=True, exiting.'); break
            time.sleep(getattr(mod, 'INTERVAL', 5))
    else:
        try: import readline
        except Exception: pass
        agent.inc_out = True
        if sys.stdout.isatty():
            try: model = agent.get_llm_name(model=True) or '?'
            except Exception: model = '?'
            try:
                sys.stdout.write(f'\x1b[92m✦\x1b[0m \x1b[1mGenericAgent\x1b[0m '
                                 f'\x1b[90m· cli · model:\x1b[0m {model}\n')
                sys.stdout.flush()
            except Exception: pass
        while True:
            q = input('> ').strip()
            if not q: continue
            try:
                dq = agent.put_task(q, source='user')
                while True:
                    item = dq.get()
                    if 'next' in item: print(item['next'], end='', flush=True)
                    if 'done' in item: print(); break
            except KeyboardInterrupt:
                agent.abort(); print('\n[Interrupted]')
