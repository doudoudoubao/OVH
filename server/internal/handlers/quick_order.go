package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/catalog"
	"github.com/ovh-buy/server/internal/numconv"
	"github.com/ovh-buy/server/internal/ovh"
	"github.com/ovh-buy/server/internal/price"
	"github.com/ovh-buy/server/internal/types"
)

// quickOrderMu 串行化 quick-order 入队的逻辑,避免并发同 plan@dc 重复入队
var quickOrderMu sync.Mutex

// recentSuccessWindowSec 防重复扣款窗口:这么多秒内已经成功买到同一份
// (plan, 机房, 配置),再来的入队请求一律拒绝。
//
// 取 120 秒是因为它要覆盖的是"同一次补货窗口内的重复触发" ——
// OVH 补货时库存常在几十秒里反复闪(有货→没货→有货),监控每次跳变都会
// 触发一批下单,而这几批买到的是同一个东西。跨补货窗口的正常复购不受影响:
// 库存真的断档再回来,一般远超两分钟。
const recentSuccessWindowSec = 120

// hasRecentSuccess 判断 recentSuccessWindowSec 秒内有没有成功买过同一份
// (planCode, 机房, 配置指纹)。调用方必须已持有 state.HistoryMu。
//
// 从尾部往前扫:history 是按时间追加的,最近的成功订单在末尾,
// 命中时立刻返回,不必遍历整份历史(抢一晚上能攒出上万条)。
//
// 解不出时间戳的条目按"不算数"处理而不是"算重复"。PurchaseTime 早期是用
// types.NowISO() 写的、不带时区,ParseTS 已经兼容两种格式;万一将来又冒出
// 第三种格式,宁可漏挡一次(用户能看见多下的单并取消),也不要凭一条解不开的
// 旧记录把真正的抢购永久挡住 —— 后者的症状是"监控在跑但从来不下单",没人查得出来。
func hasRecentSuccess(history []types.PurchaseHistoryEntry, planCode, datacenter, fp string, nowTS int64) bool {
	for i := len(history) - 1; i >= 0; i-- {
		h := history[i]
		if h.PlanCode != planCode || h.Datacenter != datacenter || h.Status != "success" {
			continue
		}
		if fingerprint(h.Options) != fp {
			continue
		}
		t, ok := types.ParseTS(h.PurchaseTime)
		if !ok {
			continue
		}
		if nowTS-t.Unix() < recentSuccessWindowSec {
			return true
		}
	}
	return false
}

// QuickOrder POST /api/queue/quick-order
// 监控触发的"立即下单"或者外部主动调用:验证账户 + 拉一次价格 → 直接塞队列头(高优先级 + 2 秒重试)。
// 这条端点是 monitor.batchOrder() auto-order 触发时走的 HTTP 路径。
func QuickOrder(state *app.State) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			AccountID          string   `json:"account_id"` // 必填,哪个账户下单
			PlanCode           string   `json:"planCode"`
			Datacenter         string   `json:"datacenter"`
			Options            []string `json:"options"`
			FromMonitor        bool     `json:"fromMonitor"`
			SkipDuplicateCheck bool     `json:"skipDuplicateCheck"`
			AutoPay            bool     `json:"autoPay"`
			// MaxMonthly / MaxMonthlyCurrency 月费上限,只有"程序自己决定买什么"的
			// 路径才会带(新机型自动下单)。0 = 不限,也是人手下单的默认 ——
			// 用户从界面挑型号时已经看见价格了,不需要这道闸。
			// 真正的判定在 PurchaseServer 结账前(见 purchase/pricecap.go):
			// addon 要到下单那一刻才从有货的 FQN 推出来,这里还不知道最终配置。
			MaxMonthly         float64 `json:"maxMonthly"`
			MaxMonthlyCurrency string  `json:"maxMonthlyCurrency"`
		}
		_ = c.ShouldBindJSON(&body)
		if body.PlanCode == "" || body.Datacenter == "" {
			c.JSON(http.StatusOK, gin.H{"success": false, "error": "缺少 planCode 或 datacenter"})
			return
		}
		if body.AccountID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "缺少 account_id"})
			return
		}
		if _, ok := state.FindAccount(body.AccountID); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "account_id 不存在"})
			return
		}
		options := body.Options
		if len(options) == 0 {
			availByConfig := catalog.CheckServerAvailabilityWithConfigs(state, body.PlanCode, body.AccountID)
			// cfg.Datacenters 的 key 是 OVH 原样返回的 AvailabilityDatacenterEnum(孟买是 ynm),
			// 而 body.Datacenter 是前端显示码(孟买是 mum),必须和 purchase / price 一样先转换
			dcKey := ovh.ConvertDisplayDCToAPIDC(body.Datacenter)
			// ConfigAvailability 用三个字段表达三种完全不同的"没配上":
			//   CatalogError + CatalogMissing=false → OVH 目录拉取失败(网络/429),重试有用
			//   CatalogMissing=true                 → 目录拉到了,但本子公司目录没这个 planCode,
			//                                         此时 CatalogError 里已经是完整的人话说明
			//   OptionsNote                         → 目录和 plan 都在,只是这套内存/存储在
			//                                         本子公司买不到(实测 US 35 条、EU/CA 各 18 条)
			// 以前这三种被一律拼成"OVH 目录拉取失败："+原文,于是 CatalogMissing 那条
			// 会显示成"OVH 目录拉取失败：XX 子公司的目录里没有 planCode YY",自相矛盾。
			catalogErr := ""
			catalogMissingMsg := ""
			optionsNote := ""
			dcSeen := false
			for _, cfg := range availByConfig {
				if cfg.CatalogMissing && catalogMissingMsg == "" {
					catalogMissingMsg = cfg.CatalogError
				} else if cfg.CatalogError != "" && !cfg.CatalogMissing && catalogErr == "" {
					catalogErr = cfg.CatalogError
				}
				dcStatus, ok := cfg.Datacenters[dcKey]
				if !ok || !catalog.IsAvailableForOrder(dcStatus) {
					continue
				}
				dcSeen = true
				if len(cfg.Options) > 0 {
					options = append(options, cfg.Options...)
					break
				}
				// 这条 FQN 在用户要的机房确实有货,却一个可下单 addon 都没匹配上 ——
				// OptionsNote 说的就是这件事,照实转达比报"无可定价配置"准确得多
				if cfg.OptionsNote != "" && optionsNote == "" {
					optionsNote = cfg.OptionsNote
				}
			}
			if len(options) == 0 {
				// 目录没拉下来时 cfg.Options 恒为空,报"无可定价配置"会把 OVH 瞬断
				// 误导成"这台机器没配置可买",几种情况必须分开说
				err := "指定机房无可定价配置（" + body.PlanCode + "@" + body.Datacenter + "）"
				switch {
				case catalogMissingMsg != "":
					// 原文已经是完整的人话(哪个子公司、哪个站点、为什么),不要再加前缀
					err = catalogMissingMsg
				case catalogErr != "":
					err = "OVH 目录拉取失败，无法匹配可下单配置：" + catalogErr
				case optionsNote != "":
					err = optionsNote
				case len(availByConfig) == 0:
					// availByConfig 为空 = /dedicated/server/datacenter/availabilities 返回了空数组。
					// 拿别的站点的 planCode 查同样是 HTTP 200 + [](实测 24rise01-v1 在 US 站点 n=0),
					// 和"这台机器暂时没货"长得一样 —— 交给 classifyPlan 分辨到底是哪种。
					if _, hint := catalog.ClassifyPlan(state, body.AccountID, body.PlanCode, "quick_order"); hint != "" {
						err = hint
					}
				case !dcSeen:
					// 机型有记录,但用户要的机房这一刻没有任何一条 FQN 可下单 = 单纯没货
					err = "机型 " + body.PlanCode + " 在机房 " + body.Datacenter + " 当前无货"
				}
				state.Logger.Warn("[quick_order] "+err, "quick_order")
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err})
				return
			}
		}

		priceResult := price.GetInternal(state, body.AccountID, body.PlanCode, body.Datacenter, options)
		if !priceResult.Success {
			err := priceResult.Error
			if err == "" {
				err = "价格查询失败"
			}
			state.Logger.Warn("快速下单前价格校验失败: "+body.PlanCode+"@"+body.Datacenter+" - "+err, "quick_order")
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "价格校验失败：" + err})
			return
		}
		if priceResult.Degraded {
			// 询价时有必填配置没设上,同样的配置在 purchase.go 是 fail-fast,
			// 放进队列只会浪费一次抢购尝试
			state.Logger.Warn("快速下单前价格校验降级，拒绝入队: "+priceResult.DegradedReason, "quick_order")
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "购物车配置不完整，暂不支持下单：" + priceResult.DegradedReason})
			return
		}
		if priceResult.Price == nil {
			state.Logger.Warn("快速下单前价格校验失败: price字段缺失", "quick_order")
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "价格查询返回数据格式异常：缺少price字段"})
			return
		}
		withTaxRaw, _ := priceResult.Price.Prices["withTax"]
		if withTaxRaw == nil {
			state.Logger.Warn("快速下单前价格缺失或无效: "+body.PlanCode+"@"+body.Datacenter, "quick_order")
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "该组合暂无有效价格，暂不支持下单"})
			return
		}
		if f, ok := numconv.ToFloat64(withTaxRaw); ok && f == 0 {
			state.Logger.Warn("快速下单前价格缺失或无效: "+body.PlanCode+"@"+body.Datacenter, "quick_order")
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "该组合暂无有效价格，暂不支持下单"})
			return
		}

		// 入队前两道闸。它们拦的是两件不同的事,所以能不能跳过也不一样:
		//
		//   闸门 A「队列里已有同款任务」—— 监控批量下单必须跳过。
		//      订阅的 quantity=N 的语义就是"同一个 plan@dc 下 N 单",
		//      不跳过的话第 2..N 个任务会被第 1 个挡掉,quantity 直接失效。
		//
		//   闸门 B「刚刚已经成功买过同款」—— 谁都不能跳过,包括监控。
		//      它拦的是重复扣款。以前 SkipDuplicateCheck 把 A 和 B 一起关了,
		//      于是唯一无人值守的那条路(监控自动下单 + autoPay)恰好没有这层保护:
		//      库存在 120 秒内抖两次(有货→没货→有货),就是两次跳变、两批单、两台机器。
		//      而 B 不会影响 quantity:同一批 N 个请求是在任何一单成交**之前**
		//      并发进来的,history 里还没有 success,N 个全部放行;
		//      它只挡"上一批真的买成了、紧接着又来一批"。
		quickOrderMu.Lock()
		defer quickOrderMu.Unlock()
		fp := fingerprint(options)

		if !(body.FromMonitor && body.SkipDuplicateCheck) {
			state.QueueMu.Lock()
			for _, it := range state.Queue {
				if it.PlanCode == body.PlanCode && it.Datacenter == body.Datacenter &&
					(it.Status == "running" || it.Status == "pending" || it.Status == "paused") &&
					fingerprint(it.Options) == fp {
					state.QueueMu.Unlock()
					state.Logger.Info("检测到重复的队列任务（含配置），拒绝再次入队", "quick_order")
					c.JSON(http.StatusTooManyRequests, gin.H{"success": false, "error": "已存在相同配置的购买任务，稍后再试"})
					return
				}
			}
			state.QueueMu.Unlock()
		} else {
			state.Logger.Info("来自监控的批量下单，跳过队列去重（quantity 语义所需）", "quick_order")
		}

		state.HistoryMu.Lock()
		recentDup := hasRecentSuccess(state.History, body.PlanCode, body.Datacenter, fp, time.Now().Unix())
		state.HistoryMu.Unlock()
		if recentDup {
			state.Logger.Warn(fmt.Sprintf(
				"检测到 %d 秒内已成功下过同配置订单（%s@%s），拒绝再次入队",
				recentSuccessWindowSec, body.PlanCode, body.Datacenter), "quick_order")
			c.JSON(http.StatusTooManyRequests, gin.H{"success": false, "error": "刚刚已成功下过同配置订单，稍后再试"})
			return
		}

		now := types.NowISO()
		item := types.QueueItem{
			ID:         uuid.NewString(),
			AccountID:  body.AccountID,
			PlanCode:   body.PlanCode,
			Datacenter: body.Datacenter,
			Options:    options,
			Status:     "running",
			RetryCount: 0,
			AutoPay:    body.AutoPay,
			// MaxRetries 封顶的是**真正提交并失败的次数**(FailureCount),无货轮次不计。
			// 早先按"检查轮次"封顶是错的:监控只在 无货→有货 的跳变上重新触发 quick-order,
			// 一旦库存持续可见而下单侧连续吃 429/5xx,任务用尽轮次置 failed 后
			// 就再也没人补这一枪 —— 自动下单会静默停摆到库存先消失再回来。
			// 20 次真实失败已经足够说明不是偶发抖动;确定性错误另有 Fatal 闸门当场终止。
			MaxRetries: 20,
			// 监控触发的自动下单用单独的(更激进的)间隔,设置页可改
			RetryInterval: state.Config.QuickOrderRetryInterval(),
			CreatedAt:     now,
			UpdatedAt:     now,
			LastCheckTime: 0,
			QuickOrder:    true,
			Priority:      100,

			MaxMonthly:         body.MaxMonthly,
			MaxMonthlyCurrency: body.MaxMonthlyCurrency,
		}
		// 落库失败以前是 `_ = state.SaveQueue()` —— 直接吞掉。
		// 这条路是监控触发的自动下单:发现有货 → 建任务 → 落库失败无人知晓,
		// 重启后任务没了,而用户以为一直在抢。入队和落库必须是一件事。
		if err := state.EnqueueItems([]types.QueueItem{item}, true); err != nil {
			state.Logger.Error("自动下单任务落库失败,已撤回: "+err.Error(), "queue")
			c.JSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"error":   "任务没能写进数据库，已撤回（避免出现重启就消失的假任务）：" + err.Error(),
			})
			return
		}

		state.Logger.Info("快速下单: "+body.PlanCode+" ("+body.Datacenter+") 已加入队列", "quick_order")

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "✅ " + body.PlanCode + " (" + body.Datacenter + ") 已加入购买队列",
			"price":   priceResult.Price,
			"options": options,
		})
	}
}

// fingerprint 排序后用 "|" 连接的 options 指纹,用于队列去重
func fingerprint(opts []string) string {
	if len(opts) == 0 {
		return ""
	}
	uniq := map[string]struct{}{}
	for _, o := range opts {
		s := strings.TrimSpace(o)
		if s != "" {
			uniq[s] = struct{}{}
		}
	}
	list := make([]string, 0, len(uniq))
	for s := range uniq {
		list = append(list, s)
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1] > list[j]; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
	return strings.Join(list, "|")
}
