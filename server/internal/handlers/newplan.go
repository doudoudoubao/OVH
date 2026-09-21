package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/catalog"
	"github.com/ovh-buy/server/internal/monitor"
)

// GetNewPlanWatch GET /api/monitor/new-plan-watch
//
// 除了配置本身,还回两个前端算不出来的东西:
//   - knownPlanCount:基线里有多少机型。0 = 还没建过基线,下一轮只建基线不告警,
//     不回这个数的话用户开了开关之后看不到任何反馈,会以为坏了。
//   - currencyHint:各账户站点的计价币种。上限的币种必须和它一致,
//     否则下单闸会一律拒绝 —— 让用户自己猜是设计失败。
func GetNewPlanWatch(state *app.State, mon *monitor.Monitor) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":          "success",
			"config":          mon.NewPlanConfig(),
			"knownPlanCount":  mon.KnownPlanCount(),
			"currencyHint":    subsidiaryCurrencies(state),
			"minIntervalMins": monitor.NewPlanMinInterval,
			"maxIntervalMins": monitor.NewPlanMaxInterval,
		})
	}
}

// SetNewPlanWatch PUT /api/monitor/new-plan-watch
func SetNewPlanWatch(state *app.State, mon *monitor.Monitor) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body monitor.NewPlanWatchConfig
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "请求体格式错误"})
			return
		}

		// 开了自动下单就必须有一个能用的上限。这一层是给用户的即时反馈 ——
		// 真正的拦截在下单链路上(monitor 决策 + PurchaseServer 结账前),
		// 但让用户提交一份"开了自动买、却永远买不成"的配置然后自己去日志里找原因,
		// 是设计失败。
		if body.AutoOrder {
			if body.MaxMonthly <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error",
					"message": "开启自动下单必须设月费上限 —— 没有上限的自动下单是一张空白支票"})
				return
			}
			if strings.TrimSpace(body.Currency) == "" {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error",
					"message": "月费上限必须写明币种（15 不说是哪国的 15 就不是一个上限）；" +
						"本程序不做汇率换算，币种必须和账户所在站点的计价币种一致"})
				return
			}
		}

		saved, err := mon.SetNewPlanConfig(body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error",
				"message": "保存失败: " + err.Error()})
			return
		}

		msg := "已保存。新机型发现" + enabledWord(saved.Enabled) +
			fmt.Sprintf("，每 %d 分钟检查一次", saved.IntervalMinutes)
		if saved.AutoOrder {
			msg += fmt.Sprintf("；自动下单已开启，上限 %.2f %s/月，单轮最多 %d 条。订单不会自动付款。",
				saved.MaxMonthly, saved.Currency, saved.MaxPerRound)
		} else {
			msg += "；自动下单未开启，只推通知。"
		}
		c.JSON(http.StatusOK, gin.H{"status": "success", "message": msg, "config": saved})
	}
}

func enabledWord(v bool) string {
	if v {
		return "已开启"
	}
	return "已关闭"
}

// subsidiaryCurrencies 各账户所在站点的计价币种,给前端当提示。
//
// 走的是已经缓存的目录(不额外打 OVH);拿不到就不给提示,不猜 ——
// 猜错会让用户填一个永远通不过下单闸的币种。
func subsidiaryCurrencies(state *app.State) map[string]string {
	out := map[string]string{}
	state.AccountsMu.RLock()
	subs := map[string]struct{}{}
	for _, a := range state.Accounts {
		if s := catalog.SubsidiaryOfAccount(a); s != "" {
			subs[s] = struct{}{}
		}
	}
	state.AccountsMu.RUnlock()

	for s := range subs {
		plans, err := catalog.ListPlans(state, s, catalog.DefaultMaxAge)
		if err != nil || len(plans) == 0 {
			continue
		}
		if cur := strings.TrimSpace(plans[0].Currency); cur != "" {
			out[s] = cur
		}
	}
	return out
}
