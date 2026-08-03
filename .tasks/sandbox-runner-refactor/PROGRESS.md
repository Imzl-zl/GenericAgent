# Progress

- Shape: epic
- FinalizationStatus: active
- Truth: .tasks/sandbox-runner-refactor/SUBTASKS.csv
- Parent: (root epic)
- Current: 审查发现修复第二轮 12/12 DONE + 独立审查 7 findings 全部修复(占位符/列数/loopback 兼容/volume inspect/刷新权限/symlink 测试/非法 env 拒绝/renew 过期拒绝); 待提交
- Latest validation: Linux cross-build ./... OK; go vet ./... OK; Go 非 DB 测试全绿 + race; worker-python 109 passed; 契约+安全 16 passed; compose config OK; Docker 29.6.2 实证 volume-subpath inspect 格式; 数据库测试仍需 TEST_DATABASE_URL
- 残余风险(方案 §10 需真实 Linux 主机): runsc 运行时验证、mTLS 证书注入容器的端到端测试、真实 Sophub 端到端、六服务 compose 冒烟、共享卷跨 UID 真实读写需部署主机执行
- Next: 提交审查修复(建议独立 commit); 残余验证: TEST_DATABASE_URL 下的 DB 套件 + 真实 Docker/runsc 主机冒烟
