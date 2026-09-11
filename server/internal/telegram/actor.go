package telegram

// 发送者授权与频率限制。
//
// 收 update 只有长轮询一条路(见 poller.go),没有入站端点,
// 所以不存在"伪造来源"的问题 —— update 是我们自己用 Bot Token 拉回来的。
// 唯一要判的是「这条消息是不是配置里那个 chat 发的」。

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ovh-buy/server/internal/app"
)

const (
	// MaxTelegramBodyBytes 单次读取 Telegram 响应的上限。update 远小于此。
	MaxTelegramBodyBytes = 64 * 1024
	// UpdateIDRetentionDays update_id 幂等表保留天数
	UpdateIDRetentionDays = 7
	// ButtonTTL 一键下单按钮有效期，与旧的 messageUUIDCacheTTL 保持一致
	ButtonTTL = 24 * time.Hour
	// RateLimitWindow / RateLimitMaxPerWindow 单 chat 的处理频率上限。
	// 这个数字是按「下单」定的 —— 真正花钱的动作,宁可紧一点。
	RateLimitWindow       = 10 * time.Second
	RateLimitMaxPerWindow = 8
	// NavRateMaxPerWindow 菜单导航的上限,走单独一个桶。
	//
	// 为什么不共用上面那个:翻菜单连点六七下太正常了(总览 → 返回 → 队列 → 返回 …),
	// 8 次/10 秒转眼就撞上,而用户看到的只是一句「操作过于频繁」,
	// 完全不知道自己做错了什么。更糟的是共用一个桶时翻菜单会把下单的额度也吃掉 ——
	// 补货那一刻按不动下单按钮,原因却是刚才翻了几页菜单。
	//
	// 导航是只读的(唯一的写是取消任务,方向安全:最坏少买不会多买),
	// 放宽的代价只是多几次 Telegram API 调用。所以给它一个宽得多的桶,但仍然封顶。
	NavRateMaxPerWindow = 30
)

// IsAuthorizedActor 判断这条 update 的发送者是否是配置里那个 chat。
// 只认 config.TgChatID：
//   - 私聊：chat_id 等于配置值即可（Telegram 私聊的 chat_id 就是对方 user_id）；
//   - 群/超级群（chat_id 为负）：除 chat 匹配外，发送者必须在 TG_ALLOWED_USER_IDS 白名单里，
//     否则群里任何成员都能下单。
//
// 这层挡的是「secret 泄漏 / 兼容模式」下的越权，不是伪造来源 —— 伪造来源由 secret 挡。
func IsAuthorizedActor(state *app.State, chatID, userID interface{}) bool {
	want := normalizeID(state.Config.Get().TgChatID)
	if want == "" {
		return false
	}
	gotChat := normalizeID(idToString(chatID))
	gotUser := normalizeID(idToString(userID))

	if gotChat != "" && gotChat == want {
		if strings.HasPrefix(gotChat, "-") {
			allow := strings.TrimSpace(os.Getenv("TG_ALLOWED_USER_IDS"))
			if allow == "" {
				return false
			}
			return idInCSV(gotUser, allow)
		}
		return true
	}
	// 兼容：配置里填的是 user id，私聊时 chat_id 与之相等
	if gotUser != "" && gotUser == want && (gotChat == "" || gotChat == gotUser) {
		return true
	}
	return false
}

func idInCSV(id, csv string) bool {
	if id == "" {
		return false
	}
	for _, p := range strings.Split(csv, ",") {
		if normalizeID(strings.TrimSpace(p)) == id {
			return true
		}
	}
	return false
}

// normalizeID 去掉 @ 前缀和小数点尾巴（JSON 数字解出来是 float64）
func normalizeID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	if i := strings.Index(s, "."); i >= 0 {
		s = s[:i]
	}
	return s
}

// idToString 把 chat_id / user_id（float64 / json.Number / string）统一成字符串
func idToString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return fmt.Sprintf("%.0f", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case int:
		return fmt.Sprintf("%d", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// ChatIDString 导出给 handler 做频率限制 key
func ChatIDString(v interface{}) string { return normalizeID(idToString(v)) }

// --- 进程内频率限制 ---

type rateBucket struct {
	windowStart time.Time
	count       int
}

var (
	rateMu sync.Mutex
	// 两个独立的桶:动作(下单等)和导航(翻菜单)。分开的理由见 NavRateMaxPerWindow。
	rateByID    = map[string]*rateBucket{}
	navRateByID = map[string]*rateBucket{}
)

// allowIn 在指定的桶里做固定窗口限流。调用方必须已持有 rateMu。
func allowIn(buckets map[string]*rateBucket, id string, max int) bool {
	now := time.Now()
	b, ok := buckets[id]
	if !ok || now.Sub(b.windowStart) > RateLimitWindow {
		buckets[id] = &rateBucket{windowStart: now, count: 1}
		return true
	}
	if b.count >= max {
		return false
	}
	b.count++
	return true
}

// AllowNavRate 菜单导航的限流,和 AllowRate 各走各的桶 ——
// 翻菜单不该消耗下单的额度,反过来也一样。
func AllowNavRate(id string) bool {
	if id == "" {
		id = "unknown"
	}
	rateMu.Lock()
	defer rateMu.Unlock()
	return allowIn(navRateByID, id, NavRateMaxPerWindow)
}

// AllowRate 按 chat（取不到则 user）维度限流，返回是否放行。
func AllowRate(id string) bool {
	if id == "" {
		id = "unknown"
	}
	now := time.Now()
	rateMu.Lock()
	defer rateMu.Unlock()
	b, ok := rateByID[id]
	if !ok || now.Sub(b.windowStart) > RateLimitWindow {
		rateByID[id] = &rateBucket{windowStart: now, count: 1}
		return true
	}
	if b.count >= RateLimitMaxPerWindow {
		return false
	}
	b.count++
	return true
}
