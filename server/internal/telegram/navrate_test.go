package telegram

import "testing"

// resetRateBuckets 清空两个限流桶。
// 限流状态是包级全局的,用例之间不清就会互相干扰 ——
// 上一个用例把桶填满,下一个一上来就被限流,而失败信息指向完全无关的地方。
func resetRateBuckets() {
	rateMu.Lock()
	defer rateMu.Unlock()
	rateByID = map[string]*rateBucket{}
	navRateByID = map[string]*rateBucket{}
}

// 菜单导航和下单必须走各自的桶。
//
// 回归的是这个:两者共用一个 8 次/10 秒的桶时,翻几页菜单就会把下单额度吃光 ——
// 补货那一刻按不动下单按钮,而原因是刚才翻了菜单,没人能把这两件事联系起来。
func TestNavRateSeparateBucket(t *testing.T) {
	resetRateBuckets()
	const chat = "12345"

	// 先把导航桶用到接近上限
	for i := 0; i < NavRateMaxPerWindow; i++ {
		if !AllowNavRate(chat) {
			t.Fatalf("导航第 %d 次就被限了，上限应该是 %d", i+1, NavRateMaxPerWindow)
		}
	}
	if AllowNavRate(chat) {
		t.Error("导航超过上限后应该被限流")
	}

	// 关键:下单的额度必须一点没少
	for i := 0; i < RateLimitMaxPerWindow; i++ {
		if !AllowRate(chat) {
			t.Fatalf("翻菜单把下单额度吃掉了：下单第 %d 次就被限流", i+1)
		}
	}
	if AllowRate(chat) {
		t.Error("下单超过上限后应该被限流")
	}
}

// 反过来也一样：下单用满了不该影响看菜单
func TestOrderRateDoesNotBlockNav(t *testing.T) {
	resetRateBuckets()
	const chat = "67890"

	for i := 0; i < RateLimitMaxPerWindow; i++ {
		AllowRate(chat)
	}
	if AllowRate(chat) {
		t.Fatal("下单桶应该已经满了")
	}
	if !AllowNavRate(chat) {
		t.Error("下单被限流后仍然应该能看菜单 —— 否则用户连'为什么被限'都查不了")
	}
}

// 不同 chat 之间互不影响
func TestNavRatePerChat(t *testing.T) {
	resetRateBuckets()
	for i := 0; i < NavRateMaxPerWindow; i++ {
		AllowNavRate("chat-a")
	}
	if AllowNavRate("chat-a") {
		t.Fatal("chat-a 应该已经满了")
	}
	if !AllowNavRate("chat-b") {
		t.Error("chat-b 不该被 chat-a 的用量影响")
	}
}

// 导航桶要明显比下单桶宽，否则这次拆分就没有意义
func TestNavRateIsMoreGenerous(t *testing.T) {
	if NavRateMaxPerWindow <= RateLimitMaxPerWindow {
		t.Errorf("导航上限 %d 不比下单上限 %d 宽，拆两个桶就失去意义了",
			NavRateMaxPerWindow, RateLimitMaxPerWindow)
	}
}

// isNotModified 必须认出 Telegram 的「内容没变」，否则连点同一个按钮会刷屏。
//
// 回归的是这个:editMessageText 在新内容与当前完全一致时返回的是 ok:false,
// 把它当失败的话调用方会退回"发新消息" —— 用户点第二下不是"什么都没发生",
// 而是整屏内容又发了一遍。静态页面(帮助、空队列)必中。
func TestIsNotModified(t *testing.T) {
	hit := []string{
		"Bad Request: message is not modified",
		"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message",
		"MESSAGE IS NOT MODIFIED",
	}
	for _, d := range hit {
		if !isNotModified(d) {
			t.Errorf("没认出「内容没变」：%q", d)
		}
	}

	// 真正的失败不能被误判成功 —— 那会把编辑失败静默吞掉
	miss := []string{
		"",
		"Bad Request: message to edit not found",
		"Bad Request: MESSAGE_TOO_LONG",
		"Forbidden: bot was blocked by the user",
		"Bad Request: message can't be edited",
	}
	for _, d := range miss {
		if isNotModified(d) {
			t.Errorf("把真实错误误判成「内容没变」：%q", d)
		}
	}
}
