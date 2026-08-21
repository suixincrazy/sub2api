// Package dto provides data transfer objects for HTTP handlers.
package dto

import "github.com/Wei-Shaw/sub2api/internal/service"

// RedactCredentials 复制一份 in，剥离 service.SensitiveCredentialKeys 列出的所有敏感子键，
// 并产出一个 has_<key> 状态 map 表示哪些敏感键存在且非零值。
//
// 输入 nil 时返回 nil, nil（避免响应里出现空对象）。
// 不修改入参；调用方拿到的 out 可安全序列化进 JSON 返回前端。
func RedactCredentials(in map[string]any) (out map[string]any, status map[string]bool) {
	if in == nil {
		return nil, nil
	}
	out = make(map[string]any, len(in))
	for k, v := range in {
		if service.IsSensitiveCredentialKey(k) {
			if isCredentialValuePresent(v) {
				if status == nil {
					status = make(map[string]bool, 4)
				}
				status["has_"+k] = true
			}
			continue
		}
		out[k] = v
	}
	return out, status
}

// RevealCredentials 返回 in 的浅拷贝，敏感子键**不做剥离**——供凭证查看接口回填编辑表单。
//
// 与 RedactCredentials 相反，是全库唯一允许把 api_key / token 原文序列化给前端的路径，
// 调用方必须已通过 step-up 2FA 门控且该路由已进审计白名单。新增调用点前请先确认这两条。
//
// 输入 nil 时返回 nil（与 RedactCredentials 保持一致，避免响应里出现空对象）。
// 拷贝是浅的：顶层键值独立，嵌套 map/slice 仍与入参共享，因此返回值只可用于序列化、不可改写。
func RevealCredentials(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// isCredentialValuePresent 判断值是否"存在且非零"。空字符串、nil、false 均视为未配置；
// 其余非零类型（数字、对象、字符串等）视为已配置。
func isCredentialValuePresent(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	default:
		return true
	}
}
