# Task 6: 附件/输出统一 workspace temp/ + 安全交付 — 执行 Spec

**Goal:** 将附件、输出和文件 marker 统一到工作区 `temp/`;要求 Runner 输出独立普通文件;Platform 以受限根目录、文件描述符校验、文件类型和大小上限安全交付,不跟随符号链接。

**Decisions:** D4、D11(单文件大小限制与宿主全局磁盘监控保留)。

**Constraints:**
- Platform 只交付 `outputs/` 下的最终普通文件;拒绝设备文件、管道、目录、越界路径、交付期间被替换的文件
- Runner 若以符号链接或临时文件生成结果,必须先复制为 outputs/ 内独立普通文件
- Platform 不能读取链接目标
- 正常 DOCX/PDF/XLSX 读取与交付不受影响(文档工具是镜像能力)

**Non-goals:** 不做文档专用 RPC/队列/容器池(删除,任务 8);不做硬配额(D11)。

**Architecture:** `session_files.go` 改为工作区 temp/ 语义;`delivery_service.go` 以受限根+fd 校验交付;Worker 文件工具(session_files.py)确保普通文件;符号链接/特殊文件/逃逸/替换攻击的拒绝测试。

**Final validation:** 交付安全测试全套:符号链接拒绝、特殊文件拒绝、路径逃逸拒绝、越界拒绝、超大拒绝、替换拒绝;DOCX/PDF/XLSX 正常交付。
