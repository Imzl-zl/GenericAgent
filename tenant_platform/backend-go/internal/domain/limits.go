// Package domain task/request size limits (审查 F1 分层收敛):
// 上限常量是业务不变量, 归 domain 单一真值源; postgres(DB 写入校验)与
// api(请求校验)、application(截断) 统一引用, 不再由 infrastructure 定义。
package domain

// Application-enforced limits (not volatile DB now() checks).
const (
	MaxPromptBytes          = 64 * 1024
	MaxPersonaBytes         = 16 * 1024
	MaxTerminalErrorBytes   = 4 * 1024
	MaxToolPolicyVersionLen = 128
	MaxSourceLen            = 64
	MaxSourceInstanceLen    = 128
	MaxMessageIDLen         = 256
	// MaxTaskMediaCount 是单任务入站媒体清单条数上限(2026-08-13 多模态
	// 链路): 防恶意超长清单撑爆 TaskEnvelope/GA 首轮 payload。
	MaxTaskMediaCount = 16
)
