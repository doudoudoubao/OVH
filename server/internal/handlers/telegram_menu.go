package handlers

// Telegram 按钮菜单。
//
// 为什么要有:在此之前所有只读操作都得打字 —— /status /queue /subs /recent /accounts。
// 手机上打这些字本身不难,难的是**记住有哪些**:/help 是一大段纯文本,
// 看完就忘,下次还得再发一次 /help 翻。而这些命令全都是无参数的只读查询,
// 天生就该是按钮。
//
// 取消任务更明显:/cancel 要你手敲任务号,而任务号得先 /queue 查出来再抄一遍。
// 列出来让人点,这一整步就没了。
//
// 边界:这里**只做只读导航和取消**,不碰下单。
// 下单要走 telegram_order_buttons 那套落库的一次性 nonce(见 telegram.go 里的说明),
// 而菜单的状态只活在进程内存里 —— 两者的安全要求不是一个量级,不能混。
// 唯一的写操作是取消任务,它的方向是安全的:最坏结果是少买,不是多买。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/monitor"
	"github.com/ovh-buy/server/internal/telegram"
	"github.com/ovh-buy/server/internal/types"
)

// menuAction 是菜单回调的 action 值,和 "wf"(watch 流程) / "add_to_queue"(下单) 并列。
const menuAction = "m"

// 菜单项 key。**必须短**:Telegram 的 callback_data 硬上限 64 字节,
// 超了它不报错、按钮点了没反应(最难查的那种故障)。
// 取消任务那两个还要再带一个任务号前缀,所以 key 本身越短越好。
const (
	menuHome     = "h"  // 回主菜单
	menuStatus   = "st" // 总览
	menuQueue    = "q"  // 抢购队列
	menuSubs     = "sb" // 在盯哪些型号
	menuRecent   = "rc" // 最近抢购结果
	menuAccounts = "ac" // 账户
	menuHelp     = "hp" // 文字版帮助(打字下单的语法还是得有地方查)
	menuCancels  = "cl" // 列出可取消的任务
	menuCancelDo = "cx" // 取消某个任务(带 t=任务号前缀)
	menuCancelAs = "ca" // 取消全部 —— 二次确认页
	menuCancelAx = "cA" // 取消全部 —— 真执行
)

// taskRefLen 任务号在 callback_data 里只带前 8 位。
//
// 任务 ID 是 uuid(36 字符),整个塞进 callback_data 会让
// {"a":"m","k":"cx","t":"<36 字符>"} 逼近 64 字节上限。
// 而 cancelText 本来就是按前缀匹配的(strings.HasPrefix),8 位前缀
// 在队列这种几十条的规模下碰撞概率可以忽略,真撞上了也只是多取消一条同前缀的,
// 方向仍然是"少买"。
const taskRefLen = 8

// menuMaxRows 列表类页面最多列几行按钮。
// 手机上超过这个数就得一直划,而队列真有几十条时用控制台看更合适。
const menuMaxRows = 8

// menuCB 造菜单按钮的 callback_data。
// extra 是可选的任务号前缀,只有取消类按钮用得到。
func menuCB(key string, extra ...string) string {
	m := map[string]interface{}{"a": menuAction, "k": key}
	if len(extra) > 0 && extra[0] != "" {
		m["t"] = extra[0]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func menuBtn(text, key string, extra ...string) map[string]string {
	return map[string]string{"text": text, "callback_data": menuCB(key, extra...)}
}

// mainMenuKeyboard 主菜单。两列排布 —— 手机上一行两颗按钮的宽度正好,
// 一行一颗太浪费竖向空间,三颗则文字会被挤到换行。
func mainMenuKeyboard() map[string]interface{} {
	rows := [][]map[string]string{
		{menuBtn("📊 总览", menuStatus), menuBtn("📋 队列", menuQueue)},
		{menuBtn("👀 在盯什么", menuSubs), menuBtn("🧾 最近结果", menuRecent)},
		{menuBtn("👤 账户", menuAccounts), menuBtn("❓ 怎么下单", menuHelp)},
	}
	return map[string]interface{}{"inline_keyboard": rows}
}

// backKeyboard 详情页底部的返回栏。
// extra 里可以塞该页特有的动作(比如队列页的「取消任务」)。
func backKeyboard(extra ...map[string]string) map[string]interface{} {
	rows := [][]map[string]string{}
	if len(extra) > 0 {
		row := []map[string]string{}
		for _, b := range extra {
			row = append(row, b)
		}
		rows = append(rows, row)
	}
	rows = append(rows, []map[string]string{menuBtn("⬅️ 返回菜单", menuHome)})
	return map[string]interface{}{"inline_keyboard": rows}
}

// menuHomeText 主菜单的正文。
// 刻意保留「直接发一行文本就能下单」这句 —— 菜单是给只读操作用的,
// 下单最快的路子仍然是打字,不能让按钮把这件事藏起来。
func menuHomeText() string {
	return "🤖 OVH 抢购助手\n\n" +
		"点下面的按钮查看状态，或者直接发一行文本下单：\n" +
		"  24sk602 gra 2\n\n" +
		"盯补货用 /watch <型号>，完整语法点「❓ 怎么下单」。"
}

// SendMainMenu 发一条带主菜单的消息(/start 和 /help 用)
func sendMainMenu(state *app.State, chatID interface{}, replyToMessageID int64) {
	telegram.SendKeyboard(state, chatID, replyToMessageID, menuHomeText(), mainMenuKeyboard())
}

// cancelableTasks 当前可取消的任务(和 cancelText 的口径保持一致)
func cancelableTasks(state *app.State) []types.QueueItem {
	state.QueueMu.Lock()
	defer state.QueueMu.Unlock()
	out := make([]types.QueueItem, 0, len(state.Queue))
	for _, it := range state.Queue {
		if it.Status == "running" || it.Status == "pending" || it.Status == "paused" {
			out = append(out, it)
		}
	}
	return out
}

// taskButtonLabel 一颗任务按钮上显示什么。
// 型号@机房 是用户认得出的东西;任务号他既不认得也记不住,所以不显示 ——
// 它只存在于 callback_data 里。
func taskButtonLabel(it types.QueueItem) string {
	label := it.PlanCode
	if it.Datacenter != "" {
		label += "@" + it.Datacenter
	}
	if len(it.Options) > 0 {
		label += fmt.Sprintf("（%d 项配置）", len(it.Options))
	}
	return "✖️ " + label
}

// cancelListPage 取消任务的列表页
func cancelListPage(state *app.State) (string, map[string]interface{}) {
	tasks := cancelableTasks(state)
	if len(tasks) == 0 {
		return "队列里没有进行中的任务。", backKeyboard()
	}
	rows := [][]map[string]string{}
	shown := tasks
	truncated := false
	if len(shown) > menuMaxRows {
		shown = shown[:menuMaxRows]
		truncated = true
	}
	for _, it := range shown {
		ref := it.ID
		if len(ref) > taskRefLen {
			ref = ref[:taskRefLen]
		}
		rows = append(rows, []map[string]string{menuBtn(taskButtonLabel(it), menuCancelDo, ref)})
	}
	rows = append(rows, []map[string]string{menuBtn("🛑 全部取消", menuCancelAs)})
	rows = append(rows, []map[string]string{menuBtn("⬅️ 返回菜单", menuHome)})

	text := fmt.Sprintf("选一个要取消的任务（共 %d 个）：", len(tasks))
	if truncated {
		text += fmt.Sprintf("\n\n只列出了前 %d 个。其余的用 /cancel <任务号> 取消，"+
			"或者用「🛑 全部取消」。", menuMaxRows)
	}
	return text, map[string]interface{}{"inline_keyboard": rows}
}

// handleMenuCallback 处理菜单回调。返回 false 表示"这不是菜单回调",交给别的分支。
//
// 和 handleFlowCallback 并列,但有个关键区别:菜单**没有 token、不吃状态**。
// 每一次点击都是拿当前真实数据重新渲染一遍 —— 队列变了、订阅删了,
// 点一下就是最新的。分步流程才需要 token 记住"上一步选了什么"。
func handleMenuCallback(state *app.State, mon *monitor.Monitor, cb map[string]interface{},
	callbackObj map[string]interface{}, chatID interface{}, messageID int64) bool {

	key := strOr(callbackObj, "k", "key")
	if key == "" {
		return false
	}
	cbID := fmt.Sprintf("%v", cb["id"])

	var text string
	var kb map[string]interface{}
	toast := ""

	switch key {
	case menuHome:
		text, kb = menuHomeText(), mainMenuKeyboard()
	case menuStatus:
		text, kb = statusText(state, mon), backKeyboard()
	case menuQueue:
		// 队列页直接给一颗「取消任务」—— 看到任务和想取消它之间不该隔一次返回主菜单
		text, kb = queueText(state), backKeyboard(menuBtn("✖️ 取消任务", menuCancels))
	case menuSubs:
		text, kb = subsText(state, mon), backKeyboard()
	case menuRecent:
		text, kb = recentText(state), backKeyboard()
	case menuAccounts:
		// 注意 accountsText 有副作用:多账户时它**自己**发一条带账户切换按钮的消息,
		// 然后返回空串(见 telegram_commands.go 的实现)。那条消息正是我们想要的 ——
		// 账户切换复用 watchFlow 的 stepSwitchAccount,菜单不该重造一遍。
		// 所以空串在这里不是错误,而是"它已经回过了",走下面的 text == "" 分支。
		text, kb = accountsText(state, chatID, 0), backKeyboard()
	case menuHelp:
		text, kb = helpText(), backKeyboard()
	case menuCancels:
		text, kb = cancelListPage(state)
	case menuCancelDo:
		ref := strOr(callbackObj, "t", "task")
		if ref == "" {
			telegram.AnswerCallback(state, cbID, "按钮已失效", true)
			return true
		}
		// 复用 cancelText:它已经处理了前缀匹配、找不到、已完成等所有情况,
		// 在这里重写一遍匹配逻辑迟早会和打字入口的行为对不上
		toast = "已处理"
		result := cancelText(state, []string{ref})
		listText, listKb := cancelListPage(state)
		text, kb = result+"\n\n"+listText, listKb
	case menuCancelAs:
		n := len(cancelableTasks(state))
		if n == 0 {
			text, kb = "队列里没有进行中的任务。", backKeyboard()
			break
		}
		// 二次确认。取消全部是不可撤销的,而「🛑 全部取消」就在每个任务按钮下面一行,
		// 手机上误触的距离只有几毫米。
		text = fmt.Sprintf("确认取消全部 %d 个任务？\n\n这个操作不可撤销，已经抢到的订单不受影响。", n)
		kb = map[string]interface{}{"inline_keyboard": [][]map[string]string{
			{menuBtn(fmt.Sprintf("确认取消这 %d 个", n), menuCancelAx)},
			{menuBtn("⬅️ 算了，返回", menuCancels)},
		}}
	case menuCancelAx:
		toast = "已全部取消"
		text, kb = cancelText(state, []string{"all"}), backKeyboard()
	default:
		return false
	}

	telegram.AnswerCallback(state, cbID, toast, false)

	// 空正文 = 这个分支已经自己回过消息了(目前只有账户页)。
	// 不能继续往下走:Telegram 的 editMessageText 不接受空 text,
	// 硬发出去只会换来一次静默失败,然后 fallback 再失败一次。
	if strings.TrimSpace(text) == "" {
		return true
	}

	text = clampReply(text)
	// 就地编辑,整个菜单始终只占一条消息。
	// 编辑失败(消息超过 48 小时不让改、或内容和键盘都没变)就退回发新消息 ——
	// 宁可多一条,也不要用户点了没反应。
	if !telegram.EditMessageText(state, chatID, messageID, text, kb) {
		telegram.SendKeyboard(state, chatID, 0, text, kb)
	}
	return true
}

// menuKeyOf 从回调数据里取菜单 key,给测试和日志用
func menuKeyOf(callbackObj map[string]interface{}) string {
	if strOr(callbackObj, "a", "action") != menuAction {
		return ""
	}
	return strings.TrimSpace(strOr(callbackObj, "k", "key"))
}
