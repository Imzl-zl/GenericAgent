# GenericAgent 桌面壳 · 方案三（新前端+新桥 配 外部核）分层与实现方案

> 目标：便携包（桌面壳）永远用**自带的前端 + 桥**，把**外部一份独立 GenericAgent 的核**接进来当后端；
> 场景 a（单后端）：桌面就用外部核，response 日志天然写外部核目录，与其 TUI/IM 共用一份，**零同步代码**。
>
> 本文所有结论均已在 dd3xp/GenericAgent_Desktop `main` 与 upstream lsdefine/GenericAgent `main`(15f7eb1) 上核对过。

---

## 一、核心架构认知（为什么可行）

桌面端是**三层**，不是前后端两层：

| 层 | 内容 | 服务方式 |
|---|---|---|
| **L1 UI** | `frontends/desktop/static/`（index.html / app.js / styles.css …） | 由桥经 HTTP(127.0.0.1:14168) 提供给 WebView2 |
| **L2 桥/适配层** | `frontends/desktop_bridge.py` + `frontends/plan_state.py` + `frontends/conductor.py` | 起 HTTP/WS 服务、serve L1、驱动 L3 |
| **L3 核** | `agentmain.py` / `agent_loop.py` / `ga.py` / `llmcore.py` | 被 L2 运行时**动态 import**（`sys.path` 插入 ga_root 后 `import_module("agentmain")`） |

> 注：`cost_tracker.py` 在 `frontends/`（不是根级核），归 **L2/bundle**（见第二节、第七节）；此前本表误列入 L3,已订正。

关键：
- **exe 不含"真前端"**。exe 只内置启动页（loading/fallback/i18n）；WebView2 启动后 `navigate("http://127.0.0.1:14168/")`，主界面完全由**桥 serve 的 static** 决定。所以"换后端源"会换界面——因为界面跟着桥/源走。
- **`ga_root` 与桥所在目录（APP_DIR）本就是两个独立概念**。桥全程用 `self.ga_root`，`ensure_ga_import_path()` 已把 ga_root 插进 sys.path。→ 架构**本就支持** "桥跑自己的、核从别处接"。缺的只是"从外部喂 ga_root 的入口"。

---

## 二、分层清单：哪些走桌面端(bundle)，哪些走外部核(ga_root)

### 跟 bundle 走（桌面适配层，永远用便携包自带的新版）
| 文件/目录 | 理由 |
|---|---|
| `frontends/desktop/static/`（L1 全部 UI） | 前端只认桥的 HTTP 契约；旧目录 UI 会退回旧界面 |
| `frontends/desktop_bridge.py`（L2 桥） | 桌面适配主体；旧桥缺 `/memory/import` 等新端点 |
| `frontends/plan_state.py` | 桥的伴随件（plan 面板状态，纯 stdlib） |
| **`frontends/conductor.py`** | **深度桌面耦合**：直接读 `~/.ga_desktop_settings.json`、含 per-conductor 模型绑定/无绑定 reload/失败兜底等增量；应用 bundle 版才有全部体验 |
| `cost_tracker.py`（`import cost_tracker` 解析到 bundle/frontends） | 它 monkeypatch llmcore 的 `_record_usage`/`print`，应与桥同版本 |
| 内嵌 python + 已装依赖（含 fastapi/uvicorn） | 便携运行环境 |

### 跟 ga_root 走（纯核 + 数据，用外部那份）
| 文件/目录 | 理由 |
|---|---|
| `agentmain.py` / `agent_loop.py` / `ga.py` / `llmcore.py` | L3 核；与 upstream ~一致（见第三节） |
| `mykey.py`（模型密钥/渠道配置） | 用外部核那套真实配置 |
| `memory/` | 外部核的记忆 |
| `temp/model_responses/` | **response 日志**：`llmcore._write_llm_log` 锚定在 llmcore.py 文件位置 → 用外部核则日志天然写外部目录，与其 TUI/IM 共用一份（场景 a 零同步） |
| `temp/desktop_sessions/` | 会话数据 |
| `reflect/scheduler.py`、IM 服务(`frontends/*app*.py`) | 附属服务，随核走 |

> 归属判据：**"含桌面特化行为 / 桥的契约生产者" → bundle；"纯 agent 能力 + 用户数据" → ga_root。**
> conductor 是唯一需要特别拍板的——它在 `frontends/` 却深度桌面耦合，故归 bundle。

---

## 三、兼容性核对结论（已验证，方案成立的依据）

以 upstream/main(15f7eb1) 为基准（假设：用户旧 GA ≈ upstream，自定义为增量）：

1. **核代码 ≈ 一致**：origin/main 完整包含 upstream/main + 335 提交；四个核文件对 upstream 差异共 **9 行**，全是空白/行尾空格 + 一个通用 `extra_sys_prompt` 附加功能（桥不依赖）。**桌面没有为适配前端而 fork 核**（桥注释明写"不改 agentmain，适配放桥侧"）。
2. **桥↔核契约全绿**：桥引用的每个核符号（`put_task(query,source,images)` 返回 Queue、`next_llm(n=-1)`、`load_llm_sessions`、`current_name`、`reload_mykeys`、`_mykey_mtime`、`llmclients`、`abort`…）+ `display_queue` 的 `{next/done/turn/outputs/source}` schema，在 upstream 核**全部存在且签名/形态一致**。
3. **桌面对核唯一的改动 `current_name`**（5aadd0b，运行态模型显示）——upstream 核**逐字一致**（已回流）。
4. **conductor**：upstream 版**本就有**桌面模型联动（读 `~/.ga_desktop_settings.json`、`_apply_desktop_model` 跟随 `ui.llmNo`）；origin 只多 3 个增量强化（per-conductor 模型、无绑定 reload、失败兜底）。用外部 conductor 不会 break，只会 degrade 这几处——**这也是把 conductor 归 bundle 的原因**。
5. **反向核对**：桥依赖 dd3xp-核-独有符号 = **0**。

> 边界：以上对 upstream HEAD 成立。**某用户 ga_root 可能停在更早 upstream**，个别符号未必有 → 由"接入探针"对**真实 ga_root** 校验兜底（见下）。

---

## 四、实现方案（三块，全向后兼容）

### ① bridge 接受外部 ga_root
- `desktop_bridge.py`：`find_default_ga_root()` 改为**优先读** `--ga-root` 参数 / `GA_ROOT` 环境变量，读不到再用现有"从 APP_DIR 上溯找 agentmain.py"的派生逻辑。
- conductor 同理：桥拉起 conductor 时（现为 `ga_root/frontends/conductor.py`）改为 **spawn bundle 自带的 `conductor.py`（APP_DIR 侧）**，并通过 `GA_ROOT`/cwd 让它 import 外部核。
  - ⚠️ **需补(已复核)**：`conductor.py:11-12` 现在是 `ROOT = 自身文件的上上级目录` 硬推、**不读任何 env**。所以「spawn bundle conductor + cwd/env 指外部核」**光靠外部环境不生效**——必须同步改 `conductor.py`，让它**优先读 `GA_ROOT`** 再回退自身路径。否则 bundle conductor 只会 import 到 bundle 自己的核。

### ② Tauri 把"接入独立源码"从 project 级改成 ga_root 级
- 现状（`lib.rs`）：override 生效时直接 `spawn <override>/frontends/desktop_bridge.py` → 桥/UI/核**三层全换外部**（= 旧界面+旧桥+旧核）。
- 改为：**恒 spawn bundle 自带的 `desktop_bridge.py`**（新桥/新 UI），把外部路径作为 `GA_ROOT` 传进去。
- `set_ga_source` 保留，但语义变为"设置外部**核**目录"；`get_or_discover_config` 的第 1 步（override 优先）改为只影响 ga_root、不再改 bridge 脚本位置。

### ③ 接入前的契约探针（取代旧的弱校验）
- 现状：`set_ga_source` 只校验 `agentmain.py` + `frontends/desktop_bridge.py` 存在 → 挡不住"合法但不兼容的核"。
- 改为：对目标 ga_root 的核跑一遍**符号/签名级**探针，不齐则明确报"该后端缺 X，Y 功能不可用"，而非运行时 AttributeError。
- ⚠️ **探针清单需补全(已复核逐行)**：原文说的"约 15 点"只覆盖了**桥**对核的调用;实测 **conductor 与 cost_tracker 还额外用到一批**,必须一并纳入,否则连上后 conductor/计费才炸。完整清单(带 file:line 依据)：

  **桥→核(desktop_bridge.py)**
  - `agentmain.GenericAgent`（类存在，零参构造）— :503-505
  - `agent.run`（可作 Thread target 的阻塞循环）— :508
  - `agent.inc_out` / `agent.verbose` 可写 bool — :506-507
  - `agent.llmclient.backend.history`（**读写**，JSON 可序列化的消息 dict 列表）— :162-163, :827, :909, :923 ← 最核心数据结构
  - `agent.next_llm(int)` — :749,751；`agent.put_task(prompt, images=[])` **返回 queue.Queue** — :754
  - `display_queue` 消息 schema：`{next:str, turn:int, outputs:list[str]}` 流式 / `{done:str, outputs:list[str]}` 结束 — :765-800
  - `agent.abort()` — :704,880；`agent.get_llm_name(model=True)`→str — :2036
  - `agent.load_llm_sessions()` — :446-452
  - backend：`type(back).__name__` 含 `"Mixin"` 时读 `back.current_name`，否则 `back.name` — :644-645
  - `llmcore` 模块：`reload_mykeys()`→(dict,...)（:350,360,526）、`llmcore._mykey_mtime`（:345,439,449 置 None 触发 reload）

  **conductor→核(conductor.py，额外)**
  - `agent.llmclients`（客户端列表）— :48；`agent.llm_no` 可写 — :50；`agent.llmclient.last_tools` 可写 — :54
  - `agent.put_task(msg, source=...)` ← **注意与桥的 `images=` 是两个 kwarg,核的 `put_task` 必须两者都支持** — :264,412
  - `agent.task_queue.put("EXIT")`（Queue + "EXIT" 哨兵）— :247；`agent.no_print` 可写 — :253-255
  - `agent.handler.working`（可变 dict，键 `'key_info'`）— :280-281

  **cost_tracker→核(cost_tracker.py，bundle 侧 monkeypatch 外部 llmcore)**
  - `llmcore._record_usage(usage:dict, api_mode:str)` 存在且签名一致（被包裹）— :129,161；`api_mode∈{messages,chat_completions,responses}`
  - `llmcore` 使用**模块级 `print`**（被替换以截 `[Output] tokens=N`）— :173
  - backend 只读：`backend.context_win`(:50)、`backend.history`(:59)

  > 其中需**验签名/形态而非仅名字**的关键点：`put_task` 返回 Queue 且同时接受 `images=`/`source=`、`backend.history` 读写形态、`_record_usage(usage,api_mode)`、`llmcore.print` 为模块级可替换名。

---

## 五、附带需修（已知、与本方案相关）

- `set_ga_source` 的**无感自愈**（可选）：外部 override 起不到可用 bridge 时，运行时静默回落 bundle 自带 runtime（非破坏性，不删用户配置），避免坏 override 卡死。
- 这些与"安装报错显式化 / i18n 修复 / UCRT / memory/import rpc 恢复"是并行事项，已在 `fix/*` 分支/main 处理。

---

## 六、一句话总结

**核几乎与 upstream 一致、桌面适配全在桥侧且 conductor 的桌面集成也已在 upstream** → 方案三（bundle 出 UI+桥+conductor，ga_root 出纯核+数据）在代码兼容层面成立，日志天然统一、零同步。落地只需三处小改 + 一个接入探针，全部向后兼容。

---

## 七、可行性复核结论（逐文件核对后补充）

> 本节为对第四节实现方案的逐项落地复核。结论：**方案三成立且可行；分层机制被证实成立;补齐下列缺口后即可落地,无需改核。**

### 已证实成立的前提
1. **三层分层靠 sys.path 天然分离,免改文件**：`ensure_ga_import_path()` 只 `sys.path.insert(0, ga_root)`(desktop_bridge.py:493)——只插**ga_root 根**;`agentmain/ga/llmcore` 是根级模块 → 解析到外部核;`plan_state/cost_tracker` 是 `frontends/` 模块,靠 `APP_DIR`(=bundle/frontends,:2026)解析 → bundle。
2. **`ga_root/frontends` 全程不进 sys.path（确证 NO）**：全仓扫描 `sys.path.insert/append`,`desktop_bridge.py`/`conductor.py`/`agentmain.py`/`ga.py`/`llmcore.py` 插入的都是 ga_root 根、ga_root 上级、或 APP_DIR——**从不插 `ga_root/frontends`**。故进程内不会出现「外部旧 frontends 模块盖掉 bundle 桥/helper」的 shadowing。(仅 `tuiapp_v2.py` 插过 frontends 目录,但它不被桥/ conductor 加载。)
3. **桥不 import 计划外的 frontends 模块**：桥的 `frontends/` import 只有 `plan_state` + `cost_tracker`,与分层清单一致,无遗漏。`session_names/worldline/slash_cmds/...` 桥根本不 import(属 tui/其它前端),对本方案无关。
4. **数据落点全锚外部核(第二轮复核确认,印证"零同步")**：
   - `desktop_sessions/`、`desktop_uploads/`、`desktop_token_history.json` 走 `DEFAULT_GA_ROOT`/`manager.ga_root`(desktop_bridge.py:64,147,1301,1640,2050)→ 只要 `find_default_ga_root` 读 `GA_ROOT`,全落外部。
   - `model_responses/` 走 `llmcore._write_llm_log` 锚 **llmcore.py 自身 `__file__`**(llmcore.py:927)→ 从外部核 import 时天然写外部;`memory/` 走核 `script_dir`(agentmain.py:16,26)→ 外部。
   - 即:会话/上传/日志/记忆**全部**落外部核目录,与其 TUI/IM 共用,零同步成立。
5. **`plan_state` / `scheduler` 无耦合风险(确认)**:`plan_state` 只 import `os/re/typing`、不碰核、只处理传入 path(plan_state.py:22,359);`reflect/scheduler.py` 无 `ga_desktop_settings`、只用自身 `__file__`,归核侧、从外部 spawn、自足。
6. **`put_task` 双 kwarg 签名在核中确为一个(确认)**：`agentmain.py:117 def put_task(self, query, source="user", images=None)` —— 一个签名同时满足桥(`images=`)与 conductor(`source=`)。其余 `next_llm(n=-1)`/`load_llm_sessions`/`get_llm_name(b=None,model=False)`/`abort`/`run` 均在 `GenericAgent`,`current_name` 为 llmcore Mixin property(:974)。

### 必补缺口（补齐后可行）
1. **conductor 必须改成读 `GA_ROOT`（阻断项）**：`conductor.py:11-12` 现在 `ROOT=自身文件上上级`硬推、不读 env。方案 ① 只说「spawn bundle conductor + cwd/env 指外部核」是**不够的**——必须同步改 conductor 让它优先读 `GA_ROOT`,否则 bundle conductor 只会连到 bundle 自己的核。
2. **探针清单要扩到 conductor + cost_tracker 的符号**(见第四节③补全版)：原「约 15 点」只覆盖桥;conductor 额外用 `llmclients / llm_no / llmclient.last_tools / task_queue+"EXIT" / no_print / handler.working['key_info'] / put_task(source=)`,cost_tracker 额外依赖 `llmcore._record_usage(usage,api_mode)` / 模块级 `print` / `backend.context_win|history`。**`put_task` 必须同时支持 `images=`(桥) 和 `source=`(conductor) 两个 kwarg**——探针要验这一点。
3. **cost_tracker 归属订正**：归 bundle(它 monkeypatch 外部 llmcore 内部),其 patch 目标纳入探针(见上)。第一节架构表已订正。
4. **清理 main 上的旧实现（与本方案冲突）**：当前 `lib.rs` 的 `set_ga_source` 走的是**「spawn 外部 bridge」(= 三层全换,旧界面)** + 我加的**「同步覆盖本体桌面文件」**。方案三要求恒 spawn bundle bridge + 传 `GA_ROOT`,并**删掉同步覆盖那段**(方案三本就零同步、不改本体,覆盖是多余且侵入的)。
5. **conductor 的 IM 插件/依赖(降级项)**：bundle conductor 以自身位置加载 `frontends/conductor_im_plugins/*`(bundle 侧),而 IM 账号配置/mykey 在外部核——功能上以 bundle 插件驱动外部配置,一般可用;内嵌 python 缺包的外部附属服务(reflect/IM)优雅降级即可。
6. **探针必须"只内省、不构造"(第二轮新发现,关键)**：`import agentmain` 本身有副作用——模块级 `makedirs(memory)`、写 `global_mem.txt`(agentmain.py:26-30);`GenericAgent.__init__` 还 `makedirs(temp)` + `load_llm_sessions()`(读 mykey、建 LLM 客户端,:53-64)。故探针**不能靠 `GenericAgent()` 实例化**(会依赖 mykey、构建客户端、并改写外部核)。正确做法：
   - 在**隔离子进程**里、以目标 ga_root 跑;
   - 用 `inspect.signature` 对 `GenericAgent` **类**和模块级函数(`llmcore.reload_mykeys`/`_record_usage`)做**符号+签名**校验,**不实例化**;
   - 容忍 `import agentmain` 造成的良性脚手架(建空 memory/temp);
   - 附带收益:该 import 同时是**依赖体检**——内嵌 python 若缺外部核所需的包,import 会失败,探针即报不兼容。
7. **`GA_ROOT` 必须在桥进程启动前就位(约束)**：`DEFAULT_GA_ROOT` 及 `_WEB_UPLOAD_DIR` 等是 import 时冻结的模块级常量(desktop_bridge.py:64,1640),故 `GA_ROOT` env 需在 spawn 时即设好;「切换源」= `switch_bridge` 重启进程 → 新进程重读 env,故不存在运行时改 ga_root 的锚定错位。

### 落地顺序（建议）
① `desktop_bridge.py` + `conductor.py` 读 `GA_ROOT`/`--ga-root`(缺口1) → ② `lib.rs` 恒 spawn bundle bridge、把外部路径作 `GA_ROOT` 传入、`set_ga_source` 语义改「设外部核目录」、**删同步覆盖**(缺口4) → ③ 接入探针(第四节③补全清单,缺口2/3) → ④ 探针失败/坏 override 时静默回落 bundle(第五节)。全部向后兼容,不改核。

<!-- 生成于 2026-07-08，依据 dd3xp/GenericAgent_Desktop main 与 upstream 15f7eb1 的逐项核对 -->
