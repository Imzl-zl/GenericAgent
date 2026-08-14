# GenericAgent 项目状态

> 本文件是当前状态快照 + 最近活跃窗口，允许覆盖更新。
> 完整历史归档见 `memory/archive/`，稳定规律见 `tools.md`。

## 当前基线

- CI 门禁（分支/PR 级）：Go（vet/build/test -p 1/race 5 关键包）、Python（根 + 平台 contract/security/smoke/integration + bot_poller + worker）、Web（lint/build）全绿
- 集成测试依赖真实 PostgreSQL（`TEST_DATABASE_URL`），缺失显式失败
- **IM 多渠道完成（2026-08-10，im-channel-binding epic 6/6 DONE）**：渠道配置统一模型（channel_configs）+ 飞书/钉钉/QQ 接入（poller BotAdapter 注册表）+ im-bindings API + Web 渠道绑定页
- **IM 流式输出完成（2026-08-10，im-streaming-delivery epic 7/7 DONE）**：StreamingSender 可选接口 + scheduler 500ms 节流转发管道 + 飞书消息编辑打字机 + QQ 单聊原生流式 + 群聊收敛（只发最终结果）+ im_streaming_mode 管理开关；真实渠道冒烟待用户凭据
- **企微渠道完成（2026-08-11，commit 3023e3d2）**：WeComAdapter（wecom_aibot_sdk WS）+ 渠道绑定页企业微信卡片 + 流式判定矩阵 + delivery 回复路由（审查 C1 修复）；真实渠道冒烟待用户凭据，流式模式建议保持 off/final_only
- **MCP 治理完成（2026-08-11，commit 58b27620）**：key 平台侧注入（mcp_servers.headers，proxy 转发注入，worker 快照永不含 key）+ 每用户×每 server×周期配额（proxy 原子扣减 429）+ web JSON 直接编辑 + 用户配额面板；集成测试修复（_register_user 冗余直插 workspace，0050 不变量遗留）
- **MCP stdio 恢复完成（2026-08-12）**：stdio transport 恢复为 **Worker 沙箱内进程宿主**（主流 mcp.json：command/args 直通，预装命令 / npx -y / uvx 全支持）；runner 经 runner-control 出网（可信部署主流模型，撤销 registry 白名单）；npx/uvx/pip 共享缓存卷（GA_PKG_CACHE_VOLUME，全租户复用）；ga-runner 镜像补 node 20 LTS + uv（固定版本+sha256）；HTTP MCP 仍走 proxy（key 托管 + 配额计量）；stdio 调用不按次计量（快照签发配额门控）
- **推送审查修复完成（2026-08-12，B1/B2/Y1/Y2/Y3/Y5+三轮）**：IM 流式接线（main.go 装配时序）+ MCP 配额调度过滤接入签发路径 + proxy 扣配额后移 resolve + 双周期事务扣减 + 掩码不匹配拒绝 + **proxy JTI 预算后移（validate/consume 拆分，拒绝路径不计量，MCP+Sophub 对称）+ 配额两阶段（预检+条件扣减）**；三轮修复 B2 配额属主分裂（filter 改按 workspace owner，与 proxy 同源）+ 防御哨兵错误码；含回归测试，Go 全量+race 绿
- **错误域分类硬化完成（2026-08-12，commit 35572e5）**：业务拒绝哨兵化（ErrValidation/ErrUserNotFound/ErrUsernameExists/ErrProviderNotFound 等 6 新哨兵）+ store 23505/ErrNoRows 归类 + api writeStoreError 统一映射（400/404/409/500）+ FAILURE_POLICY.zh-CN.md（错误域分类 + 失败策略矩阵 + 审查闭环约定）；行为变化：用户名冲突 400→409、不存在 404、DB 故障 400→500
- **llmproxy 404 兼容修复（2026-08-12，commit fa41761）**：GetProvider 错误语义迁移后 loadBoundProvider 仍查 pgx.ErrNoRows 导致集成测试回归（500 替代 404），已兼容 domain.ErrProviderNotFound；教训：store 错误语义变更是行为变更，必须 grep 全部生产调用者 + 推送前跑集成测试
- **生产部署（2026-08-12）**：本机（lou@GenericAgent 服务器）compose 项目 genericagent 全套服务，`make build` 后滚动升级 platform+llm-proxy（镜像备份 :local.bak-20260812 回滚预案）；healthz/API 路由全链路验证通过
- **stdio 恢复生产部署（2026-08-12 下午）**：make build 全量重建 7 镜像（:local + :620fcf2，备份 :local.bak-stdio）+ 重建 runner-control 网络（internal→false，runner 出网生效）+ 滚动重启 platform/web/llm-proxy/sandbox-manager/bot-poller；创建生产缓存卷 genericagent_ga_pkg_cache（属主 10002 验证 ✓）；healthz 全绿、platform 日志 0 error。**部署坑（重要）**：compose.yaml 网络定义变化（internal 标志翻转）时 `docker compose up -d` 会尝试删旧网重建——旧网上有活跃端点（platform/llm-proxy）时删除失败，**且 platform 容器被拔（Exited 0）但新容器未创建**，表现为平台短暂消失。正确处理：先 `compose stop` 所有挂该网络的服务 → `docker network rm` → `compose up -d`。本次靠 Exited(0) 无状态损坏，但流程下次要直接这样做
  - 已知数据问题（非本轮引入）：channel_configs 两条 wechat 绑定（08-08 写入）明文是 iLink 凭据字符串（`xxx@im.bot:yyy`）非 JSON，RestoreActiveBots 恢复时 poller client marshal 失败（日志 ERROR bot_lifecycle: restore channel failed）——良性不阻塞启动，待用户确认后清理/迁移
- **wechat 频道恢复（2026-08-12）**：channel_configs 两条 wechat 绑定（08-08 写入，明文为 iLink 凭据字符串非 JSON）经格式迁移恢复——重加密为 `{"token": ...}`（备份 /tmp/wechat_plaintext_backup.txt）+ 重建 bot-poller 镜像（生产 poller 一直是旧 /start 契约 bot_token 顶层字段，89081f2 改 config_json 后 8-11 部署未重建 poller，两边失配才导致 marshal 失败）；恢复后两条 restored channel 无 ERROR
- **web 镜像滞后修复（2026-08-12）**：8-11 部署时 web 镜像未重建（停在 08-08 构建），8-10/8-11 的界面改动（IM 渠道绑定页/企微卡片/MCP 治理面板）全缺失，用户看到旧“微信绑定”菜单——重建后含企业微信/渠道绑定/MCP 面板；同批排查：sandbox-manager 镜像旧但代码无改动（无影响）、ga-runner 旧但 0751a6d 仅影响宿主机场景（容器内无影响）。**教训：部署必须全量重建（make build），选择性重建会漏镜像（前有 bot-poller 契约失配，后有 web 界面滞后）**
- 最后更新：2026-08-14（Phase B 生图 image_gen v1 直连形态实施：10 原子工具 + ImageGenClient + 单测 26 全绿）

## 已完成能力

- **生图能力（2026-08-14，Phase B 直连 + 托管双形态已实施）**：`image_gen` 第 10 原子工具 + llmcore `ImageGenClient`（BaseImageGenClient/OpenAIImageGenClient/resolve_image_gen，实现进 llmcore.py 严禁新增模块——沙箱 overlay 固定清单）+ OpenAI images/generations 兼容协议（同步 b64 默认路径 + url 直下兜底 + gpt-image SSE 流式可选路径，流式失败自动降级同步一次）+ outputs/ 落盘（self.cwd 相对路径 + 前置 ≤20MiB 检查）+ `[FILE:]` marker 走 Phase A 出站链；**双形态 = 配置差异一份代码**：直连（真实上游+密钥）/托管（llm-proxy `llm.image`，migration 0058 capabilities + runtime_config 下发 image_gen 块 + policy 放行 + worker marker 兜底登记 + openapi/web 表单，**平台模式生图可用**）；错误语义统一 `[Error: image_gen ...]`（绝不用 `!!!Error:`）；response_format 仅 dall-e 发送（gpt-image 恒 b64_json）。审查修复（2026-08-14）：32MiB 双闸（chunked 计数）、image-only 拒绝、双能力 provider 按 (ID,能力) 去重、migration 0059 DB CHECK。设计真值：`.tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md`（§9.7 实施记录）+ `IM_MEDIA_ARCHITECTURE.zh-CN.md` §6
- 根框架：自主执行循环 `agent_loop.py`（~100 行）+ 10 原子工具 + L0-L4 分层记忆系统 + 能力自扩展（code_run 动态装包/建工具）
- 多前端：TUI（tui_v3）、Streamlit、桌面端、IM 机器人（TG/QQ/飞书/微信/企微/钉钉）、conductor
- 租户平台：Go 后端（api/application/domain/infrastructure 分层）+ gRPC Python worker + React/Vite Web + bot poller + 契约（proto/openapi/policy）
- IM 多渠道（2026-08-10）：对话单元分桶（workspace 共享 + 每群/私聊一桶 + /new 桶级化）+ 渠道绑定（微信扫码 + 飞书/钉钉/QQ 凭据表单，凭据 JSON 加密入库）
- 企微渠道（2026-08-11）：WeComAdapter（wecom_aibot_sdk WebSocket，线程内自管 asyncio loop；入站 chatid/userid 映射——单聊 chatid==userid 判 private，空 sender 保守归群；出站 SEND_MSG markdown 承载纯文本；流式 SEND_MSG+stream 帧）+ 注册表接线 + Web 卡片（Bot ID/Bot Secret 标签，secretLabel 泛化）+ OpenAPI/文档同步
- IM 流式输出（2026-08-10）：worker Chunk → scheduler 500ms 节流合并 → StreamingSender（飞书=占位消息+PUT 编辑打字机 / QQ 单聊=原生 stream{state,id,index,reset} 帧 / 钉钉微信=仅终态）；群聊统一只发最终结果（conversation_type 判定）；stream_final_at 抑制 delivery 文本重复（文件照发）；im_streaming_mode 管理开关（默认 streaming）
- sandbox-runner 重构（Round13）：生命周期不变量结构强制、沙箱安全加固、Foundation 垂直 E2E
- MCP 治理（2026-08-11）：管理员 web 端 mcp.json 风格 JSON 直接编辑（url+headers+timeout，headers 掩码回显/更新保留原 key）；key 平台侧持有（proxy 注入，快照不含 key，日志脱敏）；每用户配额（day/month 原子扣减，无配额行默认放行，调度粗过滤+proxy 精确强制）；transport = http（默认，proxy 计量）+ stdio（Worker 沙箱内进程宿主，2026-08-12 恢复）

## 进行中 / 未完成

- **空结果静默成功修复（2026-08-12，已编码待部署）**：生产实证 QQ/飞书 14:16 收到"任务完成：任务已完成"无实际回答——上游（newapi relay/deepseek-v4-flash）退化响应（仅 summary/thinking 或空白 content）被 GA `do_no_tool` 当作正常完成（thinking 非空不算空白、`_empty_ct` 不计数），空 body 走 delivery 兜底文案；微信 14:18/14:25 同上游恢复后正常。四层修复：①ga.py do_no_tool 剥 summary/thinking 后无可见文本=空白（计 _empty_ct，3 次 LLM_FAILED）；②llmcore MixinSession 空结果（无 text/tool_use 产出）自动切换下个 session（原生降级能力此前只对 !!!Error 生效）；③worker emit_final_terminal 空 body 且无文件 → TASK_FAILED EMPTY_RESULT（中文提示）；④Go delivery 空结果不再伪装"任务完成。"（诚实文案提示重试），userVisibleTaskResult 空 fallback 不再造"任务已完成"；⑤poller QQAdapter 入站目标类型记忆（C2C/群直发，消灭私聊回复每次白打群接口刷 11255 错误日志）；测试：根 26 / worker 143 / poller 53 / Go 全量含 DB 绿；待部署（ga-runner+platform+bot-poller 三镜像重建）

- **思考外泄架构修复（2026-08-12）**：agent_loop 输出分层由 verbose 开关落实（verbose=False 只 yield 用户可见回复，不输出轮次标记/工具行/<summary>；verbose=True 完整转录不变）——不是 worker 正则补丁；实证：三渠道交付文本同源同脏（checkpoint result 铁证），飞书打字机最显眼；新增分层回归测试，已部署（platform + ga-runner digest）
- **输出分层配套（2026-08-12 审查后补强）**：display 流新增事件信号——agentmain 每轮边界推送 `{'turn': N}`（先冲刷残留文本保证 'next' 不跨轮，outputs=turn_resps[-2:] 兼容 wechatapp 消费）+ 非 verbose 工具活动 `{'tool': name}`（worker 心跳推进信号，防长工具轮被 idle reaper 误收割）；tgapp 轮次协调从解析文本标记改为事件驱动（删 8 处死代码）；qqapp 删 dead on_direct_message_create（频道私信 intent 未订阅）；lark-oapi 约束收窄 >=1.7（LogLevel.WARNING 版本绑定）；Go 流式 open 文本 TrimSpace 防护（'…' 占位不可达）；uv.lock 孤儿文件 gitignore；新增 tests/test_agentmain_stream_events.py
- **IM 流式真实渠道修复（2026-08-12）**：①worker 侧回复清洗 `ga_worker/reply_clean.py`（Turn 标记/<summary>/🛠️ 工具行/!!!Error 全去除，接入流式 chunk + 终态 final_body，降级为防御层）——飞书/QQ 不再显示思考过程；②QQ 40007「已下发内容前缀不可修改」= open 占位与累积基准不一致 → Go `BeginReply` 携带首段文本 + poller 累积基准对齐（官方文档 replace 前缀契约实证）；已部署（含 ga-runner digest 更新），待用户真实渠道复测
- **IM 优化一轮审查修复（2026-08-12 复查，已部署+推送）**：①tgapp 纯工具收尾轮全量重复 bug（空会话注入 done 累计文本 → 改为 ✅ 已完成收尾，附件不丢）+ 回归测试 tests/test_tgapp_turn_finalize.py；②conductor 子代理卡片静默退化（非 verbose 无 summary/轮次标记 → subagent 改 verbose=True 恢复卡片语义）；③ga_cli CLI 默认 --verbose（架构注释声明 CLI 属转录表面，分层后默认变静默的偏差）；④pyproject lark-oapi>=1.0→1.7（与 poller Dockerfile 对齐，LogLevel.WARNING 版本绑定）
- **旧实现漂移清理（2026-08-12）**：runner 创建/校验双面同步（protectedRunnerEnvKeys 补 GA_OVERLAY_ROOT + 4 缓存 env、inspect wantEnv 补 GA_OVERLAY_ROOT、pkg cache env 配套校验）；4 个渠道 SDK 调用面容器内逐项实证（全 PASS）；qqapp.py 旧属性名探测清理；Go 18 包 + poller 52 测试全绿，已部署
- **IM 真实渠道连通（2026-08-12 修复部署）**：bot-poller 已补 4 渠道 SDK + 修复 botpy/lark_oapi API 兼容；**QQ 已 ready（用户已加 IP 白名单 23.94.23.150，WS 连接成功）、飞书 ws started ✓**；钉钉/企微 SDK API 已静态验证兼容，无凭据未实测
- im-channel-binding epic 仅剩**真实渠道冒烟**（需用户提供飞书/钉钉/QQ/企微应用凭据；企微重点=SEND_MSG 主动流式帧是否被服务端接受）
- **image_gen 生图（2026-08-14 已编码 + 真实 key 闭环已验 + 托管形态已实施）**：直连形态 new-api 中转实测（gpt-image-2 完整 CLI 闭环 + agnes url 直下 + 五模型兼容性图谱）；**托管形态（T8.5）已落地**：llm-proxy images/generations 路由 + llm.image capability + provider 能力维度（migration 0058）+ runtime_config image_gen 块 + policy 放行 + worker marker 兜底登记 + openapi/web；Go 全量+race+worker 146+契约 41 全绿；**IM 端到端收图待部署冒烟**（需渠道凭据 + make build 全量重建 ga-runner/platform/llm-proxy/web）；流式 SSE 与 n>1 待真实上游实测（同步路径恒可用，stream 保持 False）
- im-streaming-delivery epic 仅剩**真实渠道冒烟**（需用户提供凭据；重点=飞书编辑链路 + QQ 流式帧序列参数实测）
- 有意遗留（不产生功能缺陷，已评估）：C5 delivery_service 834 行未拆（纯结构债）；bundle 多文件 SOP 平台侧不支持（sophub 平台已上线 bundle，平台 proxy 有意收窄为 single-file，若需用要加支持）
- 残余验证（需真实 Linux 主机 + Docker/runsc）：runsc 运行时、mTLS 注入、六服务 compose 冒烟、共享卷跨 UID

## 关键决策（仍有效）

- **2026-08-12（推送审查修复定案，已落地）**：①main.go 装配时序——transport 块必须在 NewScheduler 之前（Streaming 端口构造时立即断言 botTransport，值语义非延迟引用，先前声明后赋值=恒 nil 流式全链路静默失效）；②MCP 配额调度过滤唯一生产入口=签发路径（resolveMCPSnapshot 后 filterMCPServersByQuota，死代码教训：有定义有单测不等于接入调用链）；③proxy 配额扣减在 resolve 白名单之后（404 不烧配额）；④ConsumeMCPQuotas 单事务双周期（day→month 固定锁序，要么都扣要么都不扣，被拒调用不烧 day）；⑤掩码合并不匹配/新键掩码 → 400 拒绝（不落库）；⑥**proxy 计量原则（Y5）**：JTI 预算只在"调用即将发起"时消费——validate 与 consume 拆分（authenticate 拆 validateToken+consumeBudget），客户端错误/白名单 404/配额 429/系统 503 路径不计量，上游 502 与 fetch 后判定视为已发起；MCP 与 Sophub 两 proxy 对称实现；**配额两阶段（二轮）**：quotaCheck 只读预检先于 JTI 消费、quotaConsume 条件扣减在 JTI 之后——任一拒绝路径零扣减，极端竞态白扣选短期 JTI（用户配额从不错扣）

- **2026-08-11（MCP 治理定案，已落地）**：MCP 配置 = mcp.json 风格 JSON 直接编辑（web 端），存储保留 DB（mcp_servers.headers，无独立 key 字段）；key 平台侧持有（proxy 注入，admin API 掩码回显，更新掩码值保留原 key）；配额 = 每用户 × 每 server × 周期（day/month），proxy 每次调用原子扣减（429 MCP_QUOTA_EXCEEDED），调度层按用户粗过滤耗尽 server；stdio 分发（2026-08-12 已落地）= Worker 沙箱内进程宿主（command/args 直通）+ npx/uvx/pip 共享缓存卷 + runner 出网（撤销 registry 白名单/受控代理：可信部署下过度设计）；pandoc 保持镜像预装 CLI 直调（不启用 MCP 协议）；PDF 引擎保持 pandoc→docx→LibreOffice 渲染式（不引 TeX Live）。设计真值：`.tasks/mcp-governance/`

- **2026-08-11（企微渠道定案，已落地）**：接入形态 = 企业微信智能机器人（wecom_aibot_sdk WebSocket 长连接，与飞书/钉钉/QQ 同模式，无公网回调地址）；凭据 bot_id/secret **复用 app_id/app_secret 存储槽位**（契约字段不变，Web 标签与前端必填文案随渠道泛化）；conversation_type = chatid==userid 判 private（空 sender 保守归群）；出站统一 SEND_MSG（文本用 markdown 承载——SDK 主动发送无 text 类型，被动 reply_stream 依赖入站 req_id 不适合异步 delivery）；流式 = SEND_MSG+stream 帧（协议层与被动 reply_stream 同格式，**待真实凭据实测，未通过前 im_streaming_mode 建议 off/final_only**）；新渠道加入必须同步：Go IsValidChannelType/IsValidSource/channelTypeForTaskSource/StreamForwarder 矩阵 + poller VALID_CHANNEL_TYPES/工厂 + OpenAPI 枚举 + Web ChannelType/卡片
- CI 门禁分支/PR 级矩阵；集成测试必须真实 Postgres（`-p 1` 串行化规避 flaky）
- 生命周期不变量以结构强制（文件拆分）；跨语言契约（proto/openapi）为单一真值源
- 身份模型只有一类主体——用户（Bearer）；AdminToken（`PLATFORM_ADMIN_TOKEN`）仅管理面 + `/v1/router/messages` 服务入口
- **2026-08-10（IM 流式输出定案，im-streaming-delivery 已落地）**：按渠道分档实现流式（非统一路径）——飞书=消息编辑打字机（占位消息+PUT 全量替换+5 QPS 令牌桶）、QQ 单聊=官方原生流式接口（stream{state 1/10,id,index,reset} 帧序列+msg_id 被动锚点+append≤2 保护 4 次/条限制）、QQ 群聊/钉钉/微信=仅终态；群聊统一收敛只发最终结果（tasks.conversation_type 判定，入站契约新增字段）；stream_final_at 标记流式 commit 成功，delivery 文本 part 跳过（文件照发）；失败路径无标记 → delivery 兜底补发（IM 消息不可撤回）；im_streaming_mode=off|final_only|streaming 管理开关（默认 streaming，私聊默认开）；scheduler StreamingSender 可选接口 nil=关。设计真值：`tenant_platform/docs/IM_STREAMING_DELIVERY.zh-CN.md`。**QQ 流式接口参数与飞书编辑链路需真实凭据实测（残余风险）**。
- **2026-08-10（IM 多渠道定案，im-channel-binding 已落地）**：渠道配置统一模型——`bots` → `channel_configs`（migration 0053，RENAME 后 FK 自动跟随；存量微信行 channel_type='wechat'；每用户每渠道一行；(owner_id, channel_type) 唯一）；凭据 JSON 密文（微信={token}、新渠道={app_id, app_secret}），API 只回脱敏值；新渠道属主即 canonical user（无二次绑定）；群消息触发=平台硬规则（钉钉/QQ 必须 @，飞书只申请 group_at_msg，不申请收全部的敏感权限）；Poller BotAdapter 注册表 + SDK 惰性导入；**契约字段 ilink_user_id → channel_account_id（B3 命名债已清）**。设计真值：`tenant_platform/docs/IM_CHANNEL_BINDING.zh-CN.md` + `IM_CHANNEL_ARCHITECTURE.zh-CN.md`（会话模型已落地）。
- **2026-08-10（IM 多渠道会话模型，im-channel-session 已落地）**：数据隔离单元 = workspace；对话上下文按"对话单元"分桶（桶 key = `channel:chat_id`；微信固定单桶 `wechat:me`）；`/new` 清当前桶（conversation_resets 表）；TaskEnvelope.conversation_id=14；GA 核心零改动。
- **2026-08-06（Round16）**：GA 侧故障语义统一——`_retry_or_exit` 无 fatal 分支；`total_cd_tokens` 只累计本条消息增量；API 错误契约：队列满 429 / 越权 403 / 会话不存在 404；session key 解析唯一入口 `domain.ValidateWorkspaceKey`；team id 只接受 uuid
- **2026-08-06（Round16）**：poller 合并语义唯一实现在 `InboundCoalescingBuffer`——`coalesce_webhook_bodies` 已收敛为恒等 finalize，新增合并逻辑必须改 buffer 而非该函数
- **2026-08-06（Round15）**：workspace hash 推导/校验唯一实现在 `domain.WorkspaceDirHash`；`ctrl:` 控制 JTI 约定真值在 worker.proto TaskEnvelope.capability_jti；配额命名统一 per-requester；活跃任务状态集合/claim lease 谓词真值在 postgres 包常量
- **2026-08-06（Round15）**：上限常量真值在 `domain/limits.go`；UTF-8 安全截断唯一实现在 `domain.TruncateUTF8`；心跳部署契约 `PROGRESS_WINDOW_S(150) > llm-proxy 120s < TASK_IDLE_TIMEOUT_SECONDS(300)`；provider routing 竞争窗口（409→TASK_FAILED）是有意的 fail-closed 设计
- 2026-08-06（D1 去分级）：工具能力统一静态 policy manifest；2026-08-06（D2-D5）：成功路径 draining 闭合、BlockUser 会话撤销、delivery fencing、DB 时钟 lease、LLM_FAILED 结构化终态

## 仍需注意的坑点

- Python 3.14 与 pywebview 不兼容，用 3.11/3.12
- **main.go 装配时序坑（2026-08-12 审查 B1 修复）**：Scheduler.Streaming 端口在 NewScheduler 构造时立即断言 botTransport——该变量若在构造后才赋值（哪怕是同函数内后面几行），断言恒 nil、IM 流式静默失效且单测全绿；装配类注入必须保证赋值先于构造，或改用 setter/延迟引用
- **死代码坑（2026-08-12 审查 B2 修复）**：filterMCPServersByQuota 有定义+单测但生产调用链从未接线——写功能后 grep 一下生产路径是否有调用点（`rg "函数名" --glob '!*_test.go'`）
- **测试硬编码平台命令/路径（2026-08-11 新增）**：`_list_python_pids` 曾硬编码 Windows tasklist 导致 Linux CI E2E 必失败（已加 /proc 分支）；`/media` 等宿主根目录在测试中无写权限，用 tmp_path
- **CI Python job 前置步骤失败会跳过 Worker/bot_poller/E2E（2026-08-11 新增）**：contract/smoke 失败时后续步骤全 skip，worker 断言过期可长期未被发现——本地需手动补跑 `pytest tenant_platform/worker-python -q` 与 bot_poller；bot_poller QQ 流式测试硬依赖 qq-botpy（CI 已补装）
- runsc/mTLS/真实 Docker 验证只能在 Linux 主机
- **`postgres.OpenTestPool` 有全局互斥锁，同一测试内二次调用死锁**——测试数据直插用 pgx 单连接
- **openapi platform.yaml 不要用 yaml.safe_dump 重写**——会丢全部中文注释并破坏 test_contract_sources 的文本格式断言；补路径用文本插入
- **git checkout 单文件会丢失未提交修改**——回滚前确认工作区版本是否含未提交变更
- 契约测试 `test_route_contract.py` 拦截后端新增路由未同步 OpenAPI；KNOWN_SPEC_GAPS 已清空，新增路由必须进 spec
- **表改名迁移坑（2026-08-10 新增）**：0003 的 marker 就是 bots 表本身——0053 RENAME 后必须重建仅作 marker 的 stub `bots` 表（否则 0003 被重放重建空表）；RENAME/列改名/约束改名无 IF NOT EXISTS，用 DO 块条件执行（0052 先例）
- **集成测试勿直插 workspace（2026-08-11 新增）**：注册路径已同事务自动创建 personal workspace（0050 生命周期不变量：users ⇔ personal:<uid> 行）——测试直插 `INSERT INTO workspaces ... personal:{uid}` 必唯一键冲突；需要时用 `ON CONFLICT (session_key) DO NOTHING` 幂等兑底
- **Docker Desktop 容器内进程会被 SIGKILL（2026-08-11 实证）**：Windows Docker Desktop 下容器主进程/exec 跑长任务（pytest+go build）反复 `7 Killed`（非 OOM，MemAvailable 充足）；docker 只适合短命令验证（如基线复现），长跑集成测试用 Windows 本地
- **新渠道回复路由坑（2026-08-11，审查 C1）**：`delivery_service.channelTypeForTaskSource` 是独立的 source→channel 映射——加渠道枚举/白名单/流式矩阵后**必须同步它**，漏掉会导致任务回复错投微信（跨渠道泄漏）或死信；独立审查子代理抓到的，自己验证矩阵全绿仍漏此路径（测试只覆盖 Enabled() 未覆盖 openReply/delivery 解析）
- **poller BotManager.start 构造必须在锁外**（2026-08-10）：WeChatAdapter 构造回调 coalesce_window_provider → 同一非重入锁死锁
- **凭据 upsert 前捕获旧配置**（2026-08-10）：ON CONFLICT DO UPDATE 覆盖 bot_uuid 后无法再取旧 UUID 停 poller 会话
- **微信绑定坑（2026-08-08）**：① `ILINK_BASE_URL` 为空时现路由已无条件注册返回 `501 FEATURE_DISABLED`；② iLink get_qrcode_status 是长轮询——status 请求每尝试 8s 封顶且超时不重试，PollStatus 超时返回 DB 最近状态（200 wait）
- **微信入站聚合（2026-08-08）**：`InboundCoalescingBuffer` 合并谓词必须是绝对时间间隔（`abs(a-b) <= window`），勿改回严格递增；文件+文字任务里 text 与 media_paths 都要处理
- **微信交付（2026-08-08）**：平台 delivery 层 `cleanIMMarkdown` 降级纯文本是平台职责；文档转换用 runner 镜像本地 pandoc/python-docx，勿恢复 MCP pandoc；`memory-template/wechat_delivery_sop.md` 新工作区自动预置
- **ga-runner 镜像坑（2026-08-08）**：镜像命名统一 `genericagent-*` 前缀；`docker compose build ga-runner` 产物名固定 `genericagent-ga-runner:local`；sandbox-manager 启动时 fail-fast 校验镜像存在；重建流程 `make build` → `make runner-digest` → 重启 sandbox-manager
- **runner-control 网络名坑**：实际名 `genericagent_runner-control`，用 `GA_RUNNER_NETWORK` 可配置
- **MCP 网络（2026-08-12 修订）**：runner 经 runner-control 出网（可信部署主流模型，已部署生效 internal=false）；外部 HTTP MCP Server 仍走 Platform 受控 proxy（key 托管 + 配额计量）；改 MCP 相关需同时重建 platform + ga-runner 镜像
- **MCP stdio 链路（2026-08-12 恢复后）**：stdio server 由 Worker 沙箱内 MCPStdioClient 进程宿主（subprocess + 新行分隔 JSON-RPC 2024-11-05）；不变量 ①快照携带 transport/command/args（mcp_config 严格拒绝未知字段）②command 校验=格式层（裸名/绝对路径，无 shell 元字符；管理员可信，无白名单/黑名单）③共享缓存卷 env=NPM_CONFIG_CACHE/UV_CACHE_DIR/PIP_CACHE_DIR（docker_cli 注入）④进程生命周期=deadline 封口 + close 终止（超时回收）⑤stdio 调用不经 proxy：不按次计量，配额只按快照签发门控 ⑥spawn 失败必须包成 MCPRuntimeError（session_lifecycle 捕获列表才有兜底）⑦**proxy 字段只对 http server 生效**（mcp_config use_proxy 分支；stdio 携带 proxy_base_url 会触发校验拒绝→整个快照加载失败，推送审查修复）
- **runsc DNS 坑**：gVisor netstack 与 Docker 内嵌 DNS 不兼容——manager 注入 `--add-host`（docker inspect 输出是 JSON 数组）
- **MCP 工具被策略过滤坑**：foundation.v1.json 的 allowed_tools 静态白名单——策略文件加 `mcp:*` 通配，两处过滤逻辑都要支持
- **部署配置注意**：`PER_REQUESTER_RUNNING_LIMIT`（旧名 PER_TENANT_RUNNING_LIMIT 失效）；`PLATFORM_ADMIN_*`（旧名 PLATFORM_DEV_* 失效）
- **改 proto 注释必须重跑 `generate_bindings.py`**
- **SQL 谓词收敛陷阱**：runner_leases 表有 status 列（多表查询必须保留别名前缀）
- **心跳基线**：`last_progress_at` 以 put_task 时刻为基线，勿改回 0.0
- **boundMsg/truncateBytes 已删**：截断统一用 `domain.TruncateUTF8(s, limit)`
- `.tasks/*/SUBTASKS.csv` 字段内含逗号须引号包裹

## 最近活跃窗口

- 2026-08-13：**媒体管道设计文档落地（IM_MEDIA_ARCHITECTURE.zh-CN.md）**：入站理解已落地段全量归档（TaskEnvelope.media/migration 0056/media_content_blocks/code_run 防护/OCR 依赖）；出站媒体（Phase A：基类 send_media + 5 渠道上传适配——现状仅微信 send_file 实现，飞书/QQ/钉钉/企微全为 NotImplementedError）、生图（Phase B：image_gen 工具 + llm-proxy images/generations 代理 + capability llm.image）、视频（Phase C：ffmpeg 抽帧 + 出站 send_media(video)）三期为设计稿；渠道媒体机制矩阵为官方文档实证（QQ 入站 attachments[] 直接带 URL、出站需上传拿 file_info；飞书 resource API；钉钉/企微 media API）
- 2026-08-13：**IM 多模态结构化链路（架构化，替代补丁）**：补丁版（agent_loop 正则扫 prompt 文本逆向）升级为全链路结构化——**契约先行**：proto TaskEnvelope 加 `repeated MediaItem media = 15`（alias/original_name/relative_path/size_bytes）+ migration 0056 tasks.media jsonb 持久化（路由时 ImportInbound 附件清单随任务落库）+ Go dispatch 原样下发 + worker `put_task(prompt, images=[relative_path...])` + GA `put_task(images=)` 原生参数补完（死参数实现，run() 消费→media_content_blocks 注入首轮 content blocks，PIL 降采样 1568px 控视觉 token，文本正则降级兜底）。实证：同图任务 306s 超时 → 补丁 187s → 架构化 **54s** 成功。验证：Go 全量含 DB + worker 144 + 契约/安全/smoke 41 + 根 32 全绿。部署：重建 ga-runner/platform 镜像 + migration 自动生效。设计原则：媒体=用户消息结构化部分（content block，主流共识），文件模式（workspace 工具访问）与消息模式（模型直接看）并存；新渠道走结构化自动多模态
- 2026-08-13：**图片任务 300s 超时根因修复（生产实证复现闭环）**：用户发图问"这是啥"→ 任务 307s TASK_INTERRUPTED 零回复。排查链：new-api 无请求 → llm-proxy 0 WARN → capability_usage 终态被撤销清理（不可作证据）→ 复现（webhook 签名注入真实微信 bot + 实时抓 runner 容器日志）抓到现场：**agnes-2.5-flash 在 code_run script 参数里生成 shell 命令（python3 -c/pip install/&&）→ GA 按纯 Python 写 .py 执行 SyntaxError → agent 无限重试 306s 超时**；且 GA 只把图片路径文本给模型（多模态模型收不到图）。修复四件套：①code_run python 模式 shell 特征检测自动降级 bash（ga.py _SHELL_STYLE_RE，含 shebang）；②tools_schema.json code_run 描述明确"纯 Python 禁 shell"；③agent_loop 首轮 user content 注入附件图片 image_url 块（base64 直传，≤3MB/3张，路径优先 temp/ 回退 cwd）；④镜像装 pillow+numpy+rapidocr-onnxruntime+libgl1（GA 自带 OCR 工具依赖）。实证：修复后同任务 187s 成功识别图片内容（模型还自修 vision_api.py 的 /v1 双拼）。回归测试 32 全绿。**部署坑**：用户清理镜像删了 ga-runner:local → WORKER_START_FAILED（@sha256:digest 引用在无 registry 环境拉不回镜像，GA_RUNNER_IMAGE 已改回 :local tag，tag 重建后无需重启 manager）；旧 runner 容器 idle 复用旧镜像，验证修复需先删旧容器
- 2026-08-13：**时区统一修复（北京时间）**：根因=服务器 Etc/UTC（美国 VPS 默认，非美东/美西），GA 的 Today/日志时间戳全 UTC，模型感知时间比国内慢 8h。修复：ga-runner 镜像 ENV TZ=Asia/Shanghai（zoneinfo base 自带，新 digest 3b2f35d6 已生效）+ compose 四服务（platform/bot-poller/llm-proxy/sandbox-manager）加 TZ env；**坑：alpine/docker:27-cli（golang 镜像）无 zoneinfo，TZ 环境变量静默失效仍 UTC** → 两个 Dockerfile 补 apk tzdata 重建后生效；postgres 故意不加（DB 存储 Go 侧 .UTC() 不变，web 前端 toLocaleString 浏览器本地化）；已滚动重启 5 服务 healthz 全绿。同批答疑：渠道默认 new 无配置问题——llm_nos 顺序 = is_default DESC,id ASC（默认强制第一），mixin spring_back=300s 防抖窗口内切换后直接走 go 不回弹，用户观察"一直用 go"系 00:45 new-api 停机切换后窗口内连续消息所致，间隔 >5min 自动回弹 new
- 2026-08-13：**并发参数实测调优（GA_RUNNER_MAX_ACTIVE 2→4）**：1panel 监控实证双微信任务（personal:1/personal:78 并行零排队）CPU 峰值 14%、内存增量仅 130MB（每 runner 实际 ~65MB，1GB 是护栏非占用）；GA_RUNNER_MAX_ACTIVE 仅 platform 读（lease 事务容量，sandbox-manager 不读），.env 两处已同步+语义注释；mixin 自动切换实证（00:45 双 provider 交替重试 10 次全 502 → LLM_FAILED，系用户更新 new-api 停机所致，非系统问题）；LLM_FAILED/MAX_TURNS 文案中文化；QQ 流式帧成功日志落地（open/commit 帧服务端 id 回传验证通过）
- 2026-08-12：**空结果防护已部署（2cc4f79）**：make build 全量重建（:local + :2cc4f79，备份 :local.bak-emptyfix-20260812 三个镜像）+ runner-digest 更新（新 sha256:1f3e9b0d 已生效）+ 滚动重启 platform/sandbox-manager/bot-poller；healthz 全绿、QQ WS ready、飞书 WS started、平台日志 0 error；待用户真实渠道发消息复测（含退化响应时的明确报错）

- 2026-08-12：**空结果静默成功诊断+修复（见进行中）**：生产实证链路 = 任务 4e0172b4/7beb70d6 result_digest 为空串 sha256 + committed bundle body len=0 + messages 出站"任务完成：任务已完成" + task_events 零 chunk；教训："任务完成"类文案回包先查 result digest/committed bundle，勿直接归因渠道/流式
- 2026-08-12：**MCP stdio 恢复 + runner 出网 + 并发限额调整（含推送审查修复）**：stdio 端到端恢复（domain 校验放开 + snapshot 签发携带 transport/command/args + worker MCPStdioClient + web 编辑器 + openapi）；runner-control 去 internal（可信部署主流模型，撤销 registry 白名单）；GA_PKG_CACHE_VOLUME 共享缓存卷全链路；ga-runner 镜像加 node 20 LTS + uv（sha256 校验，本地模拟验证 npx/uvx/缓存 env 生效）；并发限额 PER_REQUESTER_RUNNING_LIMIT=2 / MAX_RUNNING_TASKS=5；推送审查修复：proxy 字段仅 http server 生效（stdio 混布快照不再加载失败）+ store command 写回 nullString + proxy resolve 过滤 stdio + 安全测试 3 断言同步 + Dockerfile 预建缓存卷属主；验证：Go 全量+race 绿、worker 132 passed、契约/安全/smoke 41 passed、bot_poller 52、web lint/build 绿
- 2026-08-12：**推送审查修复（6 项+三轮 2 项全落地+回归测试）**：B1 main.go transport 装配块前移（含 botLifecycle/botPollerClient 一并提前，channelSvc Start 闭包窗口一并消除）；B2 filterMCPServersByQuota 接入 issueInitialWorkerCredentials（新测试验证签发快照不含耗尽 server）；Y1 proxy quota 后移 resolve（404 不烧配额）；Y2 ConsumeMCPQuotas 单事务双周期（新增不烧 day 回归 + 20 并发恰 limit 成功测试）；Y3 掩码不匹配/新键掩码 400 拒绝；Y5 两 proxy 拆 validateToken/consumeBudget（404/429/400 拒绝路径不烧 JTI，MCP+Sophub 对称）；Go 全量（含 DB）+ race 6 包 + api race 全绿
- 2026-08-11：**企微渠道全链路落地（3023e3d2）**：WeComAdapter + 注册表 + Web 卡片 + OpenAPI/文档；独立审查抓 C1（channelTypeForTaskSource 缺 wecom 分支→回复错投微信）已修+delivery 路由测试（fake resolver 按 channel_type 匹配真实 store 语义）；M2-M5 小修（前端校验文案/空 sender 归群/首帧占位/失败清理）；poller 52 用例 + Go TDD 全绿；已提交推送 origin/main；残余：真实凭据冒烟（SEND_MSG 流式帧验证）
- 2026-08-10：**im-streaming-delivery 全部落地**（7/7 DONE）：StreamingSender/StreamReply 接口 + StreamForwarder（500ms 节流合并 + open/append/commit/abort）+ scheduler 接入（Terminal commit + 失败 abort + 群聊收敛）+ 飞书编辑打字机 + QQ 单聊原生流式 + im_streaming_mode 开关 + Web 设置项；migration 0054（conversation_type + stream_final_at + text_value）；全量验证绿（存量失败 4 处与本次无关）；真实渠道冒烟待用户凭据
- 2026-08-10：**im-channel-binding 全部落地**（6/6 DONE）：migration 0053（bots→channel_configs）+ domain.Bot→ChannelConfig 全库改名 + im-bindings API（user+admin）+ Router 多渠道分桶（Source/ConversationKey）+ poller BotAdapter 注册表（飞书/钉钉/QQ adapter）+ Web 渠道绑定页；契约字段 ilink_user_id→channel_account_id（B3 命名债已清）；全量验证绿（存量失败 4 处与本次无关，base commit 复现）；真实渠道冒烟待用户凭据
- 2026-08-10：IM 多渠道架构落地（3 批提交）：契约 conversation_id + Go 分桶全链路 + /new 桶级化；epic im-channel-session 任务 1-5 DONE、任务 6 砍掉
- 2026-08-08：MCP stdio 接入与收敛；2026-08-06：Round17 健康清理 4 commit；Sophub 集成全链路梳理；Round16 修复；Round15 P2-P4 真值源收敛；审查 I-4 鉴权统一；全项目健康/安全审查 11 findings + 14 修复；Round13 收尾；2026-08-06：初始化项目协作文件体系
