# PROGRESS：图像能力档案化（image-capability P0/P1）

> 设计真值：同目录 `DESIGN.zh-CN.md` §8（2026-09-14 修订，取代 §4.1/§4.2 机制；§8.10 = 建档回填实测）。
> 进度真值：同目录 `SUBTASKS.csv`。

## 目标

把图像通道能力从**运行时错误文本协商**改为**声明式能力档案（数据）**，达到成熟产品形态：
逐模型声明能力 → 构造期按档案构造请求（不发已知不支持的参数）→ 语义参数不支持 fail-loud、
装饰参数省略但明示 → 协商降级为有界自愈并把结论固化成 catalog 补丁 → 工具能力描述由档案渲染。

## 硬约束（不得违反）

- 档案内联 `llmcore.py`：`tenant_platform/worker-python/src/ga_worker/runtime_overlay.py` 的
  `LEGACY_MODULES`/`LEGACY_ASSETS` 是固定白名单且进 overlay manifest digest，新增 assets 文件平台沙箱读不到。
- 语义参数（prompt/n/size/image）不静默缩水；错误前缀 `[Error: image_gen …]`，禁 `!!!Error:`。
- 预算不变式：`read_timeout < 120s`（CF 窗口）且 `(max_retries+1) × read_timeout < 300s`（任务预算）。
- 计费克制：付费模型只做零成本探测（P0/P1/P2），免费模型才补单张真图（P3，`--paid` 默认关）。

## 已完成（2026-09-14）

- P0-1…P0-6 全部落地：`_IMAGE_CATALOG`（15 条）+ `ImageChannelProfile` + `resolve_image_profile`
  优先级（配置显式 > catalog > 未建档宽松）；按档案构造 payload；语义 fail-loud / 装饰省略明示；
  每请求 model 覆盖切档案；`size_style`/`max_n`/`max_refs` 语义上限 gate；协商保留为有界自愈
  + `profile-proposal` 日志（可粘进 catalog）；schema 能力段落改为运行期由档案渲染
  （`{{IMAGE_GEN_CAPABILITIES}}` 占位 + `agentmain.load_tool_schema` 注入）。
- P1-1 建档工具 `assets/probe_image_channel.py`（P0/P1/P2 零计费；`--paid` 需显式开启）。
- P1-2 部分：12 个图像模型跑完 P0+P1，结论已回填 catalog 与 DESIGN §8.10（`size` 形态逐通道、
  sensenova 参考图 1..5、agnes 改图结论矛盾故不声明）。
- **免费模型 P3' 生产路径端到端已验（2026-09-14）**：`sensenova-u1.5-lite` 文生图 200/42.9s、
  改图 200/70.8s 且**像素客观验证**（蓝→红 changed=True）；工具层落盘/marker/省略明示/魔数改名全通。
  验证入口已固化进建档工具：`assets/probe_image_channel.py --e2e`（走 llmcore 客户端 + 像素判据）。
- **P2-2a 已完成（2026-09-14）**：参考图归一化落地在工具层（`ga._normalize_reference`）——
  源文件读取上限 64MiB（解码防御）+ **解码前**像素上限 24MP + ≤1MiB 且尺寸合规则原字节透传
  + 否则长边 ≤1568/JPEG q85（alpha 留 PNG）+ 归一化明示。真机最坏情况（33MB 噪声 PNG）→
  423KB、请求体 564KB vs 4MiB（余量 3.45MiB）、像素验证参考图仍生效；根 224 passed。
  见 DESIGN §8.12（含途中拓出的两个真缺陷）。
- P1-3 部分：`mykey.py` 已按档案形态配置（`image_gen`=agnes-image-2.5-flash；
  `image_edit`=sensenova-u1.5-lite 免费改图，**不再需要手写 protocol/operations**）。

## 验证

- `python -m pytest tests -q` → **218 passed**（`tests/test_image_gen.py` 74 → 91 例）。
- 真实渠道端到端（免费模型）：`--e2e` 生产路径 200 + 像素判据通过（见上）；证据文件
  `.tasks/image-capability/e2e_2026-09-14.json`、`probe_2026-09-14*.json`。
- 平台契约/安全/smoke：41 passed，1 例**既存环境失败**（`grpcio 1.82.1 < 生成代码要求的 1.83.0`，与本次改动无关）。
- 真实渠道：`GET /v1/models` 免费清单 + P0/P1 端点探活（0 计费）；**未跑 P3 真图**（含付费模型）。
- agentmain 注入 smoke：占位符被替换、能力块渲染、其它 9 个工具描述未动。

## 行为变更清单（必须对用户诚实）

1. catalog 已建档的模型**不再发送**已知被拒/未声明支持的装饰参数（如 `agnes-image-2.5-flash` 的
   `output_format`/`quality`）→ 原先"发出去撞 400 再裁"的用例已重指到**未建档通道**（`_UNLISTED_MODEL`）。
2. `size` 形态不符（如给 gemini 传 `1K`）现在**直接报错**，不再发出去让网关报 500。
3. 工具 schema 里写死的能力断言（"当前路由无参考图/改图能力"）被删除，改为运行期渲染。
4. sensenova 参考图上限由 1 改 5（实测）。

- **P2-1 已完成（2026-09-14）**：平台能力维度按 operation 细分——`capabilities` = chat / image.generate /
  image.edit（`image` 为别名, 写入与读取都归一化, 无数据迁移）；runtime_config 按 operation 下发
  `image_gen` / `image_edit` 两个块（各带 `operations` 声明与自己的 token）；多图像 provider 按 operation 去重；
  migration 0061 放宽 CHECK；openapi + web 同步。验证：domain + application 单测（含**跨语言 GA 探针**——
  真 GA 子进程能分别解析两个块）、web lint/build、contract/security/smoke 41 passed。
  见 DESIGN §8.13（含代理层不做 operation 细分的取舍与残余风险 P2-1b）。

## ⚠️ 环境事实（影响交付流程）

- **GitHub Actions 已关闭**（用户要求，额度问题）：`Imzl-zl/GenericAgent` 与
  `Imzl-zl/Vibe-Research-alpha` 的 `actions/permissions` = `enabled:false`。
  恢复：`gh api -X PUT repos/<owner>/<repo>/actions/permissions -F enabled=true`。
  **推论：以后没有 CI 代为验收，交付前必须本地真库跑（含迁移）。**
- 本地测试库：容器 `ga-test-pg`（postgres:16-alpine，`127.0.0.1:54329`，db=`ga_test`，
  密码 `test`）→ `TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:54329/ga_test?sslmode=disable`。

## 待办（按序）
- P2-1b（可选）：代理 operation 细分（token 签发 + 路由校验）
- P2-2b 真大图走受控上传→短时 URL（替代内联字节）
- P1-2 收尾：剩下模型的 P3 真图验证（免费模型优先；付费需用户确认预算）。
- P1-3 收尾：`mykey_template.py` 同步为档案形态（去掉 protocol/operations 手写示例）。
- P2：平台形态（契约 `capabilities` 维度 + runtime_config `operations` + 参考图进出沙箱）。
  注意：`image_gen` 走改图时 `size` 默认 1024x1024；llm-proxy 4MiB 请求体上限对参考图仍是未解约束。
