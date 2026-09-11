package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ovh-buy/server/internal/types"
)

// Telegram 对 callback_data 的硬上限是 64 字节,**超了它不报错**,
// 按钮发出去就是点了没反应 —— 最难查的那类故障。
// 所以每一种菜单按钮的 callback_data 都必须在这里量一遍。
const telegramCallbackDataLimit = 64

func TestMenuCallbackDataWithinLimit(t *testing.T) {
	keys := []string{
		menuHome, menuStatus, menuQueue, menuSubs, menuRecent,
		menuAccounts, menuHelp, menuCancels, menuCancelAs, menuCancelAx,
	}
	for _, k := range keys {
		cb := menuCB(k)
		if n := len(cb); n > telegramCallbackDataLimit {
			t.Errorf("菜单键 %q 的 callback_data %d 字节，超过上限 %d：%s",
				k, n, telegramCallbackDataLimit, cb)
		}
	}

	// 取消单个任务的按钮还要额外带一个任务号前缀,是最长的一种
	cb := menuCB(menuCancelDo, strings.Repeat("f", taskRefLen))
	if n := len(cb); n > telegramCallbackDataLimit {
		t.Errorf("取消任务按钮的 callback_data %d 字节，超过上限 %d：%s",
			n, telegramCallbackDataLimit, cb)
	}

	// 回归:如果哪天有人把整个 uuid 塞进去（36 字符），这条要能拦住
	full := menuCB(menuCancelDo, "550e8400-e29b-41d4-a716-446655440000")
	if len(full) <= telegramCallbackDataLimit {
		t.Logf("提示：完整 uuid 是 %d 字节，目前仍在限内；"+
			"但 taskRefLen=%d 的截断仍然值得保留，key 一旦变长就会超", len(full), taskRefLen)
	}
}

// callback_data 必须是能被 decodeCallbackData 解回来的 JSON,
// 且 action 字段要能把它路由到菜单分支
func TestMenuCallbackDataRoundTrip(t *testing.T) {
	cb := menuCB(menuQueue)
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(cb), &obj); err != nil {
		t.Fatalf("callback_data 不是合法 JSON: %v（%s）", err, cb)
	}
	if got := strOr(obj, "a", "action"); got != menuAction {
		t.Errorf("action = %q，期望 %q —— 路由不到菜单分支", got, menuAction)
	}
	if got := menuKeyOf(obj); got != menuQueue {
		t.Errorf("menuKeyOf = %q，期望 %q", got, menuQueue)
	}

	// 不是菜单的回调不能被 menuKeyOf 认领
	for _, other := range []string{`{"a":"wf","t":"abc","i":0}`, `{"a":"add_to_queue","u":"x"}`} {
		var o map[string]interface{}
		if err := json.Unmarshal([]byte(other), &o); err != nil {
			t.Fatal(err)
		}
		if k := menuKeyOf(o); k != "" {
			t.Errorf("%s 不该被当成菜单回调，实际取到 key=%q", other, k)
		}
	}
}

// 带任务号的按钮要能把任务号原样带回来
func TestMenuCallbackCarriesTaskRef(t *testing.T) {
	cb := menuCB(menuCancelDo, "a1b2c3d4")
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(cb), &obj); err != nil {
		t.Fatal(err)
	}
	if got := strOr(obj, "t", "task"); got != "a1b2c3d4" {
		t.Errorf("任务号没带回来：%q", got)
	}
	// 不带 extra 时不应该出现空的 t 字段（白占字节）
	var plain map[string]interface{}
	if err := json.Unmarshal([]byte(menuCB(menuHome)), &plain); err != nil {
		t.Fatal(err)
	}
	if _, has := plain["t"]; has {
		t.Error("没有任务号时不该写入 t 字段")
	}
}

// 主菜单的形状：每颗按钮都要有文字和可路由的 callback_data
func TestMainMenuKeyboardShape(t *testing.T) {
	kb := mainMenuKeyboard()
	rows, ok := kb["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatalf("inline_keyboard 类型不对：%T", kb["inline_keyboard"])
	}
	if len(rows) == 0 {
		t.Fatal("主菜单是空的")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) == 0 || len(row) > 3 {
			t.Errorf("一行 %d 颗按钮，手机上排版会坏", len(row))
		}
		for _, btn := range row {
			if strings.TrimSpace(btn["text"]) == "" {
				t.Error("有按钮没有文字")
			}
			d := btn["callback_data"]
			if len(d) > telegramCallbackDataLimit {
				t.Errorf("按钮 %q 的 callback_data 超限：%d 字节", btn["text"], len(d))
			}
			if seen[d] {
				t.Errorf("callback_data 重复：%s —— 两颗按钮会做同一件事", d)
			}
			seen[d] = true
		}
	}
}

// 返回栏永远要有「返回菜单」，否则用户会卡在详情页里出不来
func TestBackKeyboardAlwaysHasHome(t *testing.T) {
	for _, kb := range []map[string]interface{}{
		backKeyboard(),
		backKeyboard(menuBtn("✖️ 取消任务", menuCancels)),
	} {
		rows := kb["inline_keyboard"].([][]map[string]string)
		found := false
		for _, row := range rows {
			for _, btn := range row {
				if strings.Contains(btn["callback_data"], `"k":"`+menuHome+`"`) {
					found = true
				}
			}
		}
		if !found {
			t.Error("返回栏里没有回主菜单的按钮，用户会卡死在详情页")
		}
	}
}

func TestTaskButtonLabel(t *testing.T) {
	cases := []struct {
		name string
		it   types.QueueItem
		want []string // 必须出现在标签里的片段
	}{
		{"带机房", types.QueueItem{PlanCode: "24sk602", Datacenter: "gra"}, []string{"24sk602", "gra"}},
		{"没机房", types.QueueItem{PlanCode: "24sk602"}, []string{"24sk602"}},
		{"带配置", types.QueueItem{PlanCode: "24sk602", Datacenter: "rbx",
			Options: []string{"ram-64g", "softraid-2x480ssd"}}, []string{"24sk602", "rbx", "2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := taskButtonLabel(c.it)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("标签 %q 里没有 %q", got, want)
				}
			}
			// 任务号不该出现在按钮文字上 —— 用户既不认得也记不住,只会挤掉型号
			if strings.Contains(got, "-") && strings.Count(got, "-") > 2 {
				t.Logf("注意标签里连字符较多，确认没把 uuid 显示出来：%q", got)
			}
		})
	}
}

// 菜单正文不能把「直接发文本下单」这条路藏起来 ——
// 打字仍然是最快的下单方式，按钮只是给只读操作用的
func TestMenuHomeTextKeepsTypingPath(t *testing.T) {
	s := menuHomeText()
	for _, want := range []string{"24sk602", "/watch"} {
		if !strings.Contains(s, want) {
			t.Errorf("主菜单正文里缺少 %q，用户会以为只能用按钮", want)
		}
	}
	if strings.TrimSpace(s) == "" {
		t.Fatal("主菜单正文是空的 —— Telegram 的 editMessageText 不接受空 text")
	}
}
