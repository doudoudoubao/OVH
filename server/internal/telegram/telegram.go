package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ovh-buy/server/internal/app"
)

// VerifyConfig 检查 Telegram 是否可用:Token / Chat ID 是否填写 + bot 是否能 getMe + chat 是否可访问。
// 用于 AddSubscription 等"必须 TG 有效"的强制校验。
// 返回 (ok, 失败原因)。所有失败原因都是面向终端用户的中文短句。

// tokenRe 匹配 Telegram API URL 里的 bot token 段。
var tokenRe = regexp.MustCompile(`/bot[0-9]+:[A-Za-z0-9_-]+`)

// scrub 抹掉字符串里的 Bot Token。
//
// Go 的 *url.Error 文本长这样:
//
//	Post "https://api.telegram.org/bot<完整TOKEN>/sendMessage": dial tcp ...
//
// 而这类部署连 api.telegram.org 本来就常失败。以前这些 scrub(err.Error()) 被直接
// 拼进日志 —— 明文 Token 落进 logs/ 并通过 GET /api/logs 显示在前端;
// 有几处还把同一串当 error 返回,出现在「添加订阅」的报错和 webhook 信息接口里。
//
// Token 能冒充你发通知、甚至通过 webhook 触发下单,config.go 专门为它做了加密落库,
// 这条路等于把那份保护绕过去了。
func scrub(s string) string {
	return tokenRe.ReplaceAllString(s, "/bot***")
}

func VerifyConfig(state *app.State) (bool, string) {
	cfg := state.Config.Get()
	token := strings.TrimSpace(cfg.TgToken)
	chatID := strings.TrimSpace(cfg.TgChatID)
	if token == "" {
		return false, "未配置 Telegram Bot Token"
	}
	if chatID == "" {
		return false, "未配置 Telegram Chat ID"
	}
	client := &http.Client{Timeout: 10 * time.Second}

	// 1) getMe 验 token
	resp, err := client.Get("https://api.telegram.org/bot" + token + "/getMe")
	if err != nil {
		return false, "无法连接 Telegram API: " + scrub(err.Error())
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var r1 map[string]interface{}
	_ = json.Unmarshal(body, &r1)
	if ok, _ := r1["ok"].(bool); !ok {
		desc, _ := r1["description"].(string)
		if desc == "" {
			desc = "未知错误"
		}
		return false, "Telegram Token 无效: " + desc
	}

	// 2) getChat 验 chat_id (bot 是否能访问这个 chat)
	resp2, err := client.Get("https://api.telegram.org/bot" + token + "/getChat?chat_id=" + chatID)
	if err != nil {
		return false, "无法连接 Telegram API: " + scrub(err.Error())
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	var r2 map[string]interface{}
	_ = json.Unmarshal(body2, &r2)
	if ok, _ := r2["ok"].(bool); !ok {
		desc, _ := r2["description"].(string)
		if desc == "" {
			desc = "未知错误"
		}
		return false, "Telegram Chat ID 不可达: " + desc + " (请先给 bot 发一条消息)"
	}
	return true, ""
}

func SendMessage(state *app.State, message string, replyMarkup map[string]interface{}) bool {
	cfg := state.Config.Get()
	if cfg.TgToken == "" {
		state.Logger.Warn("Telegram消息未发送: Bot Token未在config中设置", "")
		return false
	}
	if cfg.TgChatID == "" {
		state.Logger.Warn("Telegram消息未发送: Chat ID未在config中设置", "")
		return false
	}

	state.Logger.Info(fmt.Sprintf("准备发送Telegram消息，ChatID: %s, TokenLength: %d", cfg.TgChatID, len(cfg.TgToken)), "")

	url := "https://api.telegram.org/bot" + cfg.TgToken + "/sendMessage"
	payload := map[string]interface{}{
		"chat_id": cfg.TgChatID,
		"text":    message,
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}

	body, _ := json.Marshal(payload)

	// 不能截断了事:"https://api.telegram.org/bot" 就占 28 字符,
	// 取前 45 位等于每发一条消息就把 Token 前 17 位记进日志。
	state.Logger.Info("发送HTTP请求到Telegram API: "+scrub(url), "")

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		state.Logger.Error("发送Telegram消息时发生未预期错误: "+scrub(err.Error()), "")
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		state.Logger.Error("发送Telegram消息时发生网络错误: "+scrub(err.Error()), "")
		return false
	}
	defer resp.Body.Close()

	state.Logger.Info(fmt.Sprintf("Telegram API响应: 状态码=%d", resp.StatusCode), "")

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		state.Logger.Info("Telegram响应数据: "+string(respBody), "")
		state.Logger.Info("成功发送消息到Telegram", "")
		return true
	}
	state.Logger.Error(fmt.Sprintf("发送消息到Telegram失败: 状态码=%d, 响应=%s", resp.StatusCode, string(respBody)), "")
	return false
}

// AnswerCallback 应答 callback_query
func AnswerCallback(state *app.State, callbackQueryID, text string, showAlert bool) {
	cfg := state.Config.Get()
	if cfg.TgToken == "" {
		return
	}
	payload := map[string]interface{}{
		"callback_query_id": callbackQueryID,
		"text":              text,
		"show_alert":        showAlert,
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+cfg.TgToken+"/answerCallbackQuery",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// SendReply 回复指定消息
func SendReply(state *app.State, chatID interface{}, text string, replyToMessageID int64) {
	cfg := state.Config.Get()
	if cfg.TgToken == "" {
		return
	}
	payload := map[string]interface{}{
		"chat_id":             chatID,
		"text":                text,
		"reply_to_message_id": replyToMessageID,
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+cfg.TgToken+"/sendMessage",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

type OrderInfo struct {
	PlanCode   string
	Datacenter string
	// AccountRef 用户在命令里显式指定的账户。原样保留,由 handlers 去解析成具体账户 ——
	// telegram 包看不到账户列表,也不该看到。
	//
	//	"@us" / "@1"  指定一个账户
	//	"@all"        同区每个能买的账户各下一单(抢稀缺机器时翻倍机会)
	//
	// 空 = 没指定,由 planCode 反推。
	AccountRef string
	// Quantity 每个机房下几台。有上限,见 MaxOrderQuantity。
	Quantity int
	Options  []string
}

// 格式: plancode [datacenter] [quantity] [options(逗号分隔)]
func ParseOrderMessage(text string) *OrderInfo {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return nil
	}
	result := &OrderInfo{
		PlanCode: parts[0],
		Quantity: 1,
	}
	remaining := []string{}
	if len(parts) > 1 {
		remaining = parts[1:]
	}
	// 先把 @账户 摘出来,它可以出现在任何位置 ——
	// 手机上打字容易顺手打在末尾,而末尾正好是 options 的地盘。
	// 摘早一点,下面的机房/数量/配置解析就完全不用知道它的存在。
	kept := remaining[:0]
	for _, p := range remaining {
		if strings.HasPrefix(p, "@") && len(p) > 1 {
			if result.AccountRef == "" {
				result.AccountRef = strings.ToLower(p[1:])
			}
			continue
		}
		kept = append(kept, p)
	}
	remaining = kept
	if len(remaining) == 0 {
		return result
	}

	// 找包含逗号的部分 = options
	optionsStart := -1
	for i, p := range remaining {
		if strings.Contains(p, ",") {
			optionsStart = i
			break
		}
	}
	if optionsStart >= 0 {
		optsText := strings.Join(remaining[optionsStart:], " ")
		for _, o := range strings.Split(optsText, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				result.Options = append(result.Options, o)
			}
		}
		remaining = remaining[:optionsStart]
	}

	switch len(remaining) {
	case 1:
		p := remaining[0]
		if n, ok := parsePositiveInt(p); ok {
			result.Quantity = clampQuantity(n)
		} else if len(p) >= 3 && len(p) <= 4 && isAllLowerAlpha(p) {
			result.Datacenter = p
		}
	case 2:
		p1, p2 := remaining[0], remaining[1]
		if len(p1) >= 3 && len(p1) <= 4 && isAllLowerAlpha(p1) {
			result.Datacenter = p1
			if n, ok := parsePositiveInt(p2); ok {
				result.Quantity = clampQuantity(n)
			}
		} else if n, ok := parsePositiveInt(p1); ok {
			result.Quantity = clampQuantity(n)
			if len(p2) >= 3 && len(p2) <= 4 && isAllLowerAlpha(p2) {
				result.Datacenter = p2
			}
		}
	}
	return result
}

// parsePositiveInt 只接受纯十进制 ASCII 数字字符串，
// 不接受 "-1" / "+5" / " 3" 等带符号或空白的版本（strconv.Atoi 会通过）。
// MaxOrderQuantity 一条聊天消息能指定的最大数量。
// 没有上限时 "planCode 4000000000" 会让 order_processor 先把 40 亿个
// QueueItem append 进一个切片 —— 进程当场 OOM 被杀。
const MaxOrderQuantity = 20

// MaxOrderFanout 一条消息最多创建多少个抢购任务。
// 不指定机房时任务数 = 配置数 × 有货机房数 × 数量,很容易远超用户直觉。
const MaxOrderFanout = 60

// clampQuantity 把数量夹到 [1, MaxOrderQuantity]
func clampQuantity(n int) int {
	if n < 1 {
		return 1
	}
	if n > MaxOrderQuantity {
		return MaxOrderQuantity
	}
	return n
}

func parsePositiveInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func isAllLowerAlpha(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) || !unicode.IsLower(r) {
			return false
		}
	}
	return len(s) > 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// MarkButtonPressed 把原消息上刚按下的那颗按钮标成已下单。
//
// 以前按完按钮，那条上架通知长得和没按过一模一样：几个机房按钮原样摆着，
// 没有任何痕迹说明哪个已经下过单了。在手机上翻回几条消息之前的通知再按一次，
// 是很自然的动作 —— 挡住它的只有服务端那道一次性 claim，
// 用户得到的反馈是一句冷冰冰的「按钮已被使用」，而他根本不记得自己按过。
//
// 这里直接用回调里带回来的原始键盘改一颗按钮的文案，不重建整个键盘 ——
// 其余机房的按钮要原样留着，用户很可能想多买几个机房。
func MarkButtonPressed(state *app.State, chatID interface{}, messageID int64, pressedData, newText string) {
	cfg := state.Config.Get()
	if cfg.TgToken == "" || messageID <= 0 {
		return
	}
	kb := rebuildKeyboard(state, chatID, messageID, pressedData, newText)
	if kb == nil {
		return
	}
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": map[string]interface{}{"inline_keyboard": kb},
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+cfg.TgToken+"/editMessageReplyMarkup",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// 编辑失败不影响下单本身,只是少了个视觉反馈
		state.Logger.Debug("标记按钮已用失败: "+scrub(err.Error()), "telegram")
		return
	}
	resp.Body.Close()
}

// EditMessageText 就地改掉一条消息的正文和键盘。
//
// 为什么需要它:按钮菜单每按一次都发一条新消息的话,翻几层聊天记录就被刷满了,
// 而用户真正想看的是"当前这一屏"。就地编辑让整个菜单始终只占一条消息。
//
// 编辑失败**不是**致命的:Telegram 对超过 48 小时的消息不允许编辑,
// 内容完全没变时也会返回 message is not modified。两种都只记 Debug ——
// 调用方随后可以自己决定要不要退回发新消息。返回值就是给它判断用的。
func EditMessageText(state *app.State, chatID interface{}, messageID int64,
	text string, replyMarkup map[string]interface{}) bool {
	cfg := state.Config.Get()
	if cfg.TgToken == "" || messageID <= 0 {
		return false
	}
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+cfg.TgToken+"/editMessageText",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		state.Logger.Debug("编辑消息失败: "+scrub(err.Error()), "telegram")
		return false
	}
	defer resp.Body.Close()
	// 和 SendKeyboard 同一个理由:callback_data 超 64 字节时 Telegram 不报错,
	// 按钮发出去就是点了没反应。非 ok 响应必须记下来,否则这种故障完全无声。
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, MaxTelegramBodyBytes))
	_ = json.Unmarshal(b, &r)
	if r.OK {
		return true
	}
	// "message is not modified" 必须当成功。
	//
	// 新正文和键盘跟当前完全一样时,Telegram 返回的是 ok:false 而不是 ok:true ——
	// 而"完全一样"在菜单里太常见了:连点两下同一个按钮、或者打开一个静态页面
	// (帮助、空队列)再点一次,就是这个结果。
	// 把它当失败的话调用方会退回"发新消息",于是用户点第二下不是"什么都没发生",
	// 而是整屏内容又发了一遍 —— 想清屏的动作反而把屏刷满了。
	//
	// 屏幕上已经是我们想要的样子,这就是成功。
	if isNotModified(r.Description) {
		return true
	}
	state.Logger.Debug("编辑消息被拒: "+r.Description, "telegram")
	return false
}

// isNotModified 判断这次编辑失败是不是"内容本来就一样"。
// 按子串匹配而不是全等:Telegram 的完整描述后面还跟着一长串解释,
// 而且这段文案历史上变过措辞。
func isNotModified(desc string) bool {
	return strings.Contains(strings.ToLower(desc), "message is not modified")
}

// storedKeyboard 由 handlers 在回调里塞进来的原始键盘(来自 callback_query.message)。
// 用一个短生命周期的 map 传递,避免给 MarkButtonPressed 加一个巨大的参数。
var (
	kbMu    sync.Mutex
	kbCache = map[string][][]map[string]interface{}{}
)

// StashKeyboard 记下这条消息当前的键盘,供随后的 MarkButtonPressed 使用。
func StashKeyboard(chatID interface{}, messageID int64, kb [][]map[string]interface{}) {
	if kb == nil {
		return
	}
	kbMu.Lock()
	kbCache[kbKey(chatID, messageID)] = kb
	// 这个 map 只在"收到回调 → 标记按钮"之间活几毫秒,但异常路径可能不取走。
	// 攒到一定量就整体清掉,不引入定时器。
	if len(kbCache) > 256 {
		kbCache = map[string][][]map[string]interface{}{
			kbKey(chatID, messageID): kb,
		}
	}
	kbMu.Unlock()
}

func kbKey(chatID interface{}, messageID int64) string {
	return fmt.Sprintf("%v:%d", chatID, messageID)
}

// rebuildKeyboard 取出原键盘,把命中的那颗按钮改文案。
func rebuildKeyboard(state *app.State, chatID interface{}, messageID int64, pressedData, newText string) [][]map[string]interface{} {
	kbMu.Lock()
	kb, ok := kbCache[kbKey(chatID, messageID)]
	delete(kbCache, kbKey(chatID, messageID))
	kbMu.Unlock()
	if !ok || kb == nil {
		return nil
	}
	hit := false
	for _, row := range kb {
		for _, b := range row {
			if d, _ := b["callback_data"].(string); d == pressedData {
				b["text"] = newText
				hit = true
			}
		}
	}
	if !hit {
		return nil
	}
	return kb
}

// BotCommands 注册给 Telegram 的命令列表。
//
// 注册之后用户在聊天框打 "/" 就会看到这个菜单,点一下就发出去 ——
// 不用记、不用打字,在手机上尤其重要。
// 这是让一个 bot 显得"有人管"最便宜的一件事,而以前一条都没注册过。
var BotCommands = []map[string]string{
	// menu 排第一:它是按钮面板的入口,点一下就能到达下面这些只读命令,
	// 用户不必再记住它们各自叫什么
	{"command": "menu", "description": "按钮菜单（状态 / 队列 / 订阅 / 取消）"},
	{"command": "help", "description": "怎么用 / 下单格式"},
	{"command": "watch", "description": "盯着补货就抢（/watch 型号 [机房] [x数量]）"},
	{"command": "unwatch", "description": "不盯了（/unwatch 型号）"},
	{"command": "status", "description": "监控与队列总览"},
	{"command": "queue", "description": "正在抢的任务"},
	{"command": "cancel", "description": "取消任务（/cancel 任务号 或 all）"},
	{"command": "subs", "description": "监控订阅列表"},
	{"command": "recent", "description": "最近的抢购结果"},
	{"command": "accounts", "description": "可用的 OVH 账户"},
}

// RegisterCommands 把命令菜单推给 Telegram。
// 幂等,启动时调一次即可;失败只是少了个菜单,不影响任何功能,所以不返回错误。
func RegisterCommands(state *app.State) {
	cfg := state.Config.Get()
	if strings.TrimSpace(cfg.TgToken) == "" {
		return
	}
	payload := map[string]interface{}{"commands": BotCommands}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+cfg.TgToken+"/setMyCommands",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		state.Logger.Debug("注册 Telegram 命令菜单失败: "+scrub(err.Error()), "telegram")
		return
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, MaxTelegramBodyBytes))
	_ = json.Unmarshal(respBody, &r)
	if r.OK {
		state.Logger.Info(fmt.Sprintf("已注册 %d 条 Telegram 命令菜单", len(BotCommands)), "telegram")
		return
	}
	state.Logger.Debug("注册 Telegram 命令菜单被拒: "+r.Description, "telegram")
}

// SendKeyboard 发一条带内联键盘的消息。
//
// SendReply 不带 reply_markup，而分步选择流程的每一步都需要按钮 ——
// 让用户在手机上点，而不是去背 ram-64g-noecc-2133 这种 addon 代码。
func SendKeyboard(state *app.State, chatID interface{}, replyToMessageID int64,
	text string, replyMarkup map[string]interface{}) {
	cfg := state.Config.Get()
	if cfg.TgToken == "" {
		return
	}
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if replyToMessageID > 0 {
		payload["reply_to_message_id"] = replyToMessageID
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+cfg.TgToken+"/sendMessage",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		state.Logger.Warn("发送带按钮的消息失败: "+scrub(err.Error()), "telegram")
		return
	}
	defer resp.Body.Close()
	// Telegram 对 callback_data 有 64 字节硬限制，超了它**不会报错**，
	// 按钮发出去就是点了没反应。这里把非 ok 响应记下来，否则这种故障完全无声。
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, MaxTelegramBodyBytes))
	_ = json.Unmarshal(b, &r)
	if !r.OK {
		state.Logger.Warn("Telegram 拒绝了带按钮的消息: "+r.Description, "telegram")
	}
}
