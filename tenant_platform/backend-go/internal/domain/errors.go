package domain

import "errors"

// 通用业务哨兵(错误域分类, 见 tenant_platform/docs/FAILURE_POLICY.zh-CN.md):
// 业务拒绝 = 客户端可修正的输入/状态问题(→ 4xx), 与基础设施故障(→ 500)
// 分离。store/service 层返回哨兵(可用 %w 包装上下文), handler 层用
// errors.Is 映射状态码; 新增业务哨兵在 api/store_errors.go 登记分支。
var (
	// ErrValidation 参数/状态校验失败(缺字段、格式错误、非法状态迁移)——400。
	ErrValidation = errors.New("validation failed")
)
