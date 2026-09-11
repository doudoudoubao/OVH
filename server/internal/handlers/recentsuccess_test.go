package handlers

import (
	"testing"
	"time"

	"github.com/ovh-buy/server/internal/types"
)

// entry 造一条历史记录。时间用 types.NowISOLayout —— 那是 NowISO() 实际写进库的
// 格式(不带时区),历史记录里绝大多数都长这样。
func entry(plan, dc, status string, opts []string, at time.Time) types.PurchaseHistoryEntry {
	return types.PurchaseHistoryEntry{
		PlanCode:     plan,
		Datacenter:   dc,
		Status:       status,
		Options:      opts,
		PurchaseTime: at.Format(types.NowISOLayout),
	}
}

// 防重复扣款闸门:窗口内买成过同一份 (plan, 机房, 配置) 就拦下。
//
// 这道闸门以前被 SkipDuplicateCheck 连同队列去重一起关掉了,而监控自动下单
// 恰恰一直带着那个标志 —— 于是唯一无人值守的那条路没有任何防重复保护。
func TestHasRecentSuccess(t *testing.T) {
	now := time.Now()
	nowTS := now.Unix()
	opts := []string{"ram-64g", "softraid-2x480ssd"}

	t.Run("窗口内买成过 → 拦", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "success", opts, now.Add(-30*time.Second)),
		}
		if !hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("30 秒前刚买成同款，应该拦下")
		}
	})

	t.Run("超出窗口 → 放行", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "success", opts, now.Add(-(recentSuccessWindowSec+10)*time.Second)),
		}
		if hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("超过窗口的历史订单不该继续拦")
		}
	})

	t.Run("失败的历史不算 → 放行", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "failed", opts, now.Add(-10*time.Second)),
		}
		if hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("失败记录不是重复扣款，不该拦")
		}
	})

	t.Run("换机房 → 放行", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "success", opts, now.Add(-10*time.Second)),
		}
		if hasRecentSuccess(h, "24sk602", "rbx", fingerprint(opts), nowTS) {
			t.Error("不同机房是不同的货，不该拦")
		}
	})

	t.Run("换配置 → 放行", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "success", opts, now.Add(-10*time.Second)),
		}
		other := []string{"ram-128g", "softraid-2x960nvme"}
		if hasRecentSuccess(h, "24sk602", "gra", fingerprint(other), nowTS) {
			t.Error("不同配置是不同的货，不该拦")
		}
	})

	t.Run("options 顺序不同视为同一配置 → 拦", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "success", []string{"softraid-2x480ssd", "ram-64g"}, now.Add(-10*time.Second)),
		}
		if !hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("fingerprint 会排序，顺序不同仍是同一配置，应该拦")
		}
	})

	t.Run("时间戳解不开 → 放行而不是误拦", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{{
			PlanCode: "24sk602", Datacenter: "gra", Status: "success",
			Options: opts, PurchaseTime: "去年某天",
		}}
		// 宁可漏挡一次(用户能看见多下的单并取消),也不要凭一条解不开的旧记录
		// 把真正的抢购永久挡住 —— 后者的症状是"监控在跑但从来不下单"。
		if hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("解不出时间戳的记录不该被当成刚刚买过")
		}
	})

	t.Run("空历史 → 放行", func(t *testing.T) {
		if hasRecentSuccess(nil, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("没有历史时不该拦")
		}
	})

	// 库里两种时间格式并存:老记录是 NowISO(不带时区),也有 RFC3339 的。
	// 两种都必须认 —— 认不出就等于这道闸门对那批记录失效。
	t.Run("RFC3339 格式的历史同样能拦", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{{
			PlanCode: "24sk602", Datacenter: "gra", Status: "success", Options: opts,
			PurchaseTime: now.Add(-20 * time.Second).Format(time.RFC3339),
		}}
		if !hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("RFC3339 时间戳的历史记录没被识别，闸门对这批记录失效")
		}
	})

	// 命中的必须是"最近那条",不能被更早的同款记录带偏
	t.Run("同款多条历史时看最近一条", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "success", opts, now.Add(-3*time.Hour)),
			entry("24sk602", "gra", "success", opts, now.Add(-5*time.Second)),
		}
		if !hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
			t.Error("最近一条在窗口内，应该拦")
		}
	})

	// quantity 的关键性质:同一批 N 个请求是在任何一单成交**之前**进来的,
	// 那时 history 里还没有对应的 success,N 个必须全部放行 ——
	// 否则这道闸门会把 quantity>1 直接废掉。
	t.Run("同批次尚未成交时不拦 → quantity 不被误伤", func(t *testing.T) {
		h := []types.PurchaseHistoryEntry{
			entry("24sk602", "gra", "pending", opts, now),
			entry("24sk602", "gra", "failed", opts, now),
		}
		for i := 0; i < 3; i++ {
			if hasRecentSuccess(h, "24sk602", "gra", fingerprint(opts), nowTS) {
				t.Fatalf("第 %d 个并发请求被误拦，quantity>1 会失效", i+1)
			}
		}
	})
}
