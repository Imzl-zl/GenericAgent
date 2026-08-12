package domain

import "errors"

// user 域业务哨兵(错误域分类, 见 tenant_platform/docs/FAILURE_POLICY.zh-CN.md):
// 业务拒绝(客户端可修正)与基础设施故障分离——handler 层 errors.Is 映射
// 为 4xx, 其余 store 错误一律 5xx, 不得降级为客户端错误。
var (
	// ErrUserNotFound 目标用户不存在(users 表无行 / 已被删除)。
	ErrUserNotFound = errors.New("user not found")
	// ErrUsernameExists 用户名已存在(唯一键冲突 23505)——409, 非基础设施故障。
	ErrUsernameExists = errors.New("username already exists")
)
