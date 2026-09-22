package purchase

import (
	"fmt"
	"math"
	"strings"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/catalog"
	"github.com/ovh-buy/server/internal/types"
)

// priceForOptions 指向 catalog.PriceForOptions。
//
// 做成包级变量是为了能在测试里换掉它 —— 这道闸有五条互斥的失败路径
// (算不出价 / 币种不符 / Partial / 超上限 / 放行),每条都得能单独构造出来,
// 而它们的差别全在这个函数返回什么。真去拉一份 12MB 的公开目录既慢又不可控。
// catalog 包自己也是这么做的(region.go 的 fetchSubsidiaryCatalog)。
var priceForOptions = catalog.PriceForOptions

// enforceMonthlyCap 结账前的月费上限闸。返回 (要返回的结果, 是否拦下)。
//
// # 为什么闸设在这里,而不是发现机型或入队的时候
//
// addon(内存 / 硬盘)不是入队时就定下的 —— 用户没指定配置时,它是在下单这一刻
// 从"当时有货的那条 FQN"推出来的(见上面 effectiveOptions 那段)。也就是说入队那一刻
// 根本不知道最终买的是哪套配置。而基础机型价**不含** addon 加价,拿它当上限判据
// 会系统性偏低:一台基础价 9 欧的机器配上 NVMe 和大内存可以轻松翻倍,
// 那样的"上限"拦不住真正贵的单。所以真正算数的这一道必须落在这里 ——
// 车已经装好、addon 已经加完、结账还没发出。
//
// # 四种情况一律不下单(fail closed)
//
//   - 算不出价:目录拉不到,或这个 plan 不在目录里。
//   - Partial:有 addon 不在目录里,PriceForOptions 明确说了这时给的是**偏低**的数字。
//     拿一个已知偏低的价去跟上限比,等于没有上限 —— 这是最隐蔽的一种失效,
//     因为它每次都"通过",而且看起来很便宜。
//   - 币种不符:不做汇率换算。EUR 的上限管不了 USD 的单,
//     而猜一个汇率去决定花不花钱是荒唐的。
//   - 超上限:本来就该拦。
//
// # 拦下来之后判不判 Fatal
//
// Fatal 在队列里意味着**直接置 failed、不再重试**,所以只能给真正确定性的失败:
//
//	超上限 / 币种不符  → Fatal。价格和币种不会因为再试一次就变,
//	                     不判 Fatal 的话任务会按 retryInterval 永远刷下去。
//	算不出价 / Partial → 不判 Fatal,只计一次失败尝试。
//	                     "目录拉不到"是暂时的(OVH 瞬断、本机网络、30 秒负缓存),
//	                     Partial 也取决于目录当下的内容 —— 判 Fatal 等于
//	                     让一次网络抖动永久杀掉一个本来能抢到的任务。
//	                     计入 FailureCount 是为了有个头(MaxRetries),
//	                     而 recordFailure 对同一个任务是**更新同一行** history,
//	                     不会刷出一堆重复记录。
//
// # 抢购主链路的开销
//
// MaxMonthly <= 0 时整段跳过。手工下单的任务全都是 0 —— 用户从界面挑型号时
// 已经看见价格了,不需要这道闸。所以补货那一刻的热路径一次多余查询都不做。
// 设了上限时走的是 catalog 那份 2 小时缓存(公开目录,不占账户配额)。
func enforceMonthlyCap(state *app.State, item *types.QueueItem, addons []string) (Outcome, bool) {
	if item.MaxMonthly <= 0 {
		return Outcome{}, false
	}

	// block 确定性失败:重试无益,直接把任务判死。
	block := func(msg string) (Outcome, bool) {
		state.Logger.Error(fmt.Sprintf("[月费上限] %s@%s 已拦下,不结账: %s",
			item.PlanCode, item.Datacenter, msg), "purchase")
		recordFailure(state, item, msg)
		return Outcome{Fatal: true, Reason: msg}, true
	}
	// retryLater 这一次算不出可信的价格,但下一次可能可以。
	// 不判 Fatal,只计一次失败尝试 —— 有 MaxRetries 兜底,不会永远刷。
	retryLater := func(msg string) (Outcome, bool) {
		state.Logger.Warn(fmt.Sprintf("[月费上限] %s@%s 本轮拦下(可重试): %s",
			item.PlanCode, item.Datacenter, msg), "purchase")
		recordFailure(state, item, msg)
		return Outcome{Attempted: true}, true
	}

	// 金额一律写成 "15.00 EUR" 这种形式,不拼货币符号:
	// 符号表在 monitor 那边(formatAmount),为一条错误文案把它复制过来不值得,
	// 而且"15.00 EUR"在任何语境下都不会被读错。
	amount := func(v float64) string {
		return fmt.Sprintf("%.2f %s", v, strings.ToUpper(strings.TrimSpace(item.MaxMonthlyCurrency)))
	}

	price, err := priceForOptions(state, item.AccountID, item.PlanCode, addons)
	if err != nil {
		// 目录拉不到多半是暂时的,下一轮再试 —— 判 Fatal 会让一次网络抖动
		// 永久杀掉一个本来能抢到的任务。
		return retryLater(fmt.Sprintf(
			"设了月费上限 %s,但这一轮算不出这套配置的价格(%s)——价格未知时不下单,下一轮会再试。",
			amount(item.MaxMonthly), err.Error()))
	}

	wantCur := strings.ToUpper(strings.TrimSpace(item.MaxMonthlyCurrency))
	gotCur := strings.ToUpper(strings.TrimSpace(price.Currency))
	if wantCur == "" || gotCur == "" || wantCur != gotCur {
		return block(fmt.Sprintf(
			"月费上限的币种是 %q,而这个账户所在站点的计价币种是 %q，两者对不上。"+
				"本程序不做汇率换算，因此不下单 —— 请按该站点的币种重新设置上限。",
			wantCur, gotCur))
	}

	if price.Partial {
		// 同样不判 Fatal:哪些 addon 在目录里是随目录内容变的,
		// 目录刷新一次就可能补齐。
		return retryLater(fmt.Sprintf(
			"设了月费上限 %s,但这套配置里有 addon(%v)不在目录里，"+
				"算出来的 %.2f 是**偏低**的数字，拿它跟上限比等于没有上限，因此本轮不下单。",
			amount(item.MaxMonthly), addons, price.Monthly))
	}

	// 目录价是两位小数,但"基础价 + 逐个 addon 累加"会留浮点尾巴
	// (7.99 + 7.01 在 float64 里是 15.000000000000002)。先四舍五入到分再比,
	// 否则"上限 15、总价正好 15"会被自己的浮点误差拦掉。
	monthly := math.Round(price.Monthly*100) / 100
	limit := math.Round(item.MaxMonthly*100) / 100
	if monthly > limit {
		return block(fmt.Sprintf(
			"这套配置月费 %s，超过设定的上限 %s，未下单。"+
				"（配置：%v；上限是在「新机型自动下单」里设的，想买请手动下单或调高上限）",
			amount(monthly), amount(limit), addons))
	}

	state.Logger.Info(fmt.Sprintf("[月费上限] %s@%s 月费 %s 未超上限 %s，放行结账（配置 %v）",
		item.PlanCode, item.Datacenter, amount(monthly), amount(limit), addons), "purchase")
	return Outcome{}, false
}
