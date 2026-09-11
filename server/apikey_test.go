package main

import "testing"

// 默认 API 密钥的判定必须看**值**,不是"环境变量是不是空的"。
//
// 回归的是这个:.env.example 里写死 API_SECRET_KEY=123456,README 让用户
// 「复制成 .env 改」。照做但漏改这一行时 os.Getenv 返回的是 "123456" 而不是空串 ——
// 旧实现只判空串,于是最可能真的在用默认密钥的那条路径恰好告警不到,
// 配上 LISTEN_HOST 默认监听所有网卡,就是一台同网段谁都能操作 OVH 账户的机器。
func TestResolveAPIKey(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantKey   string
		wantIsDef bool
	}{
		{"没设 → 用兜底值并告警", "", defaultAPIKey, true},
		{"设了但照抄示例值 → 仍要告警", defaultAPIKey, defaultAPIKey, true},
		{"照抄示例值 + 尾随空格 → 仍要告警", "  " + defaultAPIKey + "  ", defaultAPIKey, true},
		{"只有空白 → 等同没设", "   ", defaultAPIKey, true},
		{"自己设过 → 不告警", "s3cr3t-xyz", "s3cr3t-xyz", false},
		{"自己设过 + 两边空格 → 去掉空格且不告警", " s3cr3t-xyz ", "s3cr3t-xyz", false},
		// 只是包含默认值、并不等于它,不能误报 —— 误报多了用户会开始无视这条告警
		{"包含但不等于默认值 → 不告警", defaultAPIKey + "-plus", defaultAPIKey + "-plus", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, isDef := resolveAPIKey(c.raw)
			if key != c.wantKey {
				t.Errorf("resolveAPIKey(%q) key = %q，期望 %q", c.raw, key, c.wantKey)
			}
			if isDef != c.wantIsDef {
				t.Errorf("resolveAPIKey(%q) isDefault = %v，期望 %v", c.raw, isDef, c.wantIsDef)
			}
		})
	}
}

// 生效的密钥永远不能是空串:空串会让 auth 中间件把「没带 X-API-Key」
// 和「带了正确密钥」判成同一件事。
func TestResolveAPIKeyNeverEmpty(t *testing.T) {
	for _, raw := range []string{"", " ", "\t", "\n  \n"} {
		if key, _ := resolveAPIKey(raw); key == "" {
			t.Fatalf("resolveAPIKey(%q) 返回了空密钥", raw)
		}
	}
}
