package purchase

// 月费上限闸的用例。
//
// 这道闸是「新机型自动下单」唯一管钱的东西:程序自己决定买什么的时候,
// 它是用户和一台 180 欧/月的机器之间唯一的东西。所以五条路径逐条钉死,
// 而且**每条都验证过注入 bug 会变红**(见本文件末尾注释里的记录)。
//
// 复用 purchaseserver_test.go 的 fakeOVH:走的是真的 PurchaseServer,
// 假的只有 OVH 那一端和定价函数。

import (
	"context"
	"fmt"
	"testing"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/catalog"
)

// stubPricer 把 priceForOptions 换成固定返回,并记下它收到的 addon 列表。
// 返回的 *[]string 让用例能断言"拿去算价的到底是哪几个码"。
func stubPricer(t *testing.T, price catalog.PlanPrice, err error) (calls *int, gotAddons *[]string) {
	t.Helper()
	n := 0
	var seen []string
	orig := priceForOptions
	priceForOptions = func(_ *app.State, _, _ string, addons []string) (catalog.PlanPrice, error) {
		n++
		seen = append([]string{}, addons...)
		return price, err
	}
	t.Cleanup(func() { priceForOptions = orig })
	return &n, &seen
}

// eur 造一个 EUR 计价、完整(非 Partial)的报价。
func eur(monthly float64) catalog.PlanPrice {
	return catalog.PlanPrice{Monthly: monthly, Currency: "EUR"}
}

// runWithCap 跑一单带上限的下单,返回结果和那台假 OVH。
func runWithCap(t *testing.T, cap float64, currency string) (Outcome, *fakeOVH, *app.State) {
	t.Helper()
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, availPlainInStock)
	st := newPurchaseTestState(t, f.srv.URL)
	item := testItem()
	item.MaxMonthly = cap
	item.MaxMonthlyCurrency = currency
	return PurchaseServer(context.Background(), st, item), f, st
}

// assertNotOrdered 拦下时共同的后果:没结账、没报成功、history 留了原因、cart 清干净。
func assertNotOrdered(t *testing.T, out Outcome, f *fakeOVH, st *app.State) {
	t.Helper()
	if n := f.count("POST /order/cart/" + testCartID + "/checkout"); n != 0 {
		t.Errorf("上限闸该拦下的单,结账了 %d 次;序列 %v", n, f.seq())
	}
	if out.Success {
		t.Error("不该报成功")
	}
	if h, ok := historyFor(t, st, "task-1"); !ok || h.Status != "failed" || h.ErrorMessage == nil {
		t.Errorf("必须留一条写明原因的 failed，否则用户只看到任务没了却不知道为什么: %+v", h)
	}
	// 拦下也要把 OVH 那边的车清掉,否则高频触发会堆僵尸 cart
	if n := f.count("DELETE /order/cart/" + testCartID); n != 1 {
		t.Errorf("拦下后要清 cart，实际 DELETE 了 %d 次", n)
	}
}

// assertBlocked 确定性失败:重试一万次都是同一个结果,必须判 Fatal 把任务停掉。
func assertBlocked(t *testing.T, out Outcome, f *fakeOVH, st *app.State) {
	t.Helper()
	assertNotOrdered(t, out, f, st)
	if !out.Fatal {
		t.Errorf("价格/币种不会因为再试一次就变，必须判 Fatal 否则任务会按 retryInterval 永远刷: %+v", out)
	}
}

// assertRetryLater 可重试失败:这一轮算不出可信价格,但下一轮可能可以。
// 判 Fatal 会让一次网络抖动永久杀掉一个本来能抢到的任务。
func assertRetryLater(t *testing.T, out Outcome, f *fakeOVH, st *app.State) {
	t.Helper()
	assertNotOrdered(t, out, f, st)
	if out.Fatal {
		t.Errorf("目录拉不到是暂时的，判 Fatal 等于一次网络抖动就永久杀掉任务: %+v", out)
	}
	if !out.Attempted {
		t.Errorf("要计一次失败尝试，否则没有 MaxRetries 兜底、会永远刷下去: %+v", out)
	}
}

// 超上限:这是这道闸最基本的职责。
func TestMonthlyCapBlocksTooExpensive(t *testing.T) {
	stubPricer(t, eur(18.99), nil)
	out, f, st := runWithCap(t, 15, "EUR")
	assertBlocked(t, out, f, st)
}

// 正好等于上限要放行 —— "15 欧以下"里的 15 是含的。
func TestMonthlyCapAllowsExactlyAtLimit(t *testing.T) {
	stubPricer(t, eur(15), nil)
	out, f, _ := runWithCap(t, 15, "EUR")
	if !out.Success {
		t.Fatalf("正好等于上限应当放行,实际 %+v", out)
	}
	if n := f.count("POST /order/cart/" + testCartID + "/checkout"); n != 1 {
		t.Errorf("应当结账一次,实际 %d 次", n)
	}
	f.waitForCall(t, "GET /me/order/"+testOrderID)
}

// 浮点尾巴不能把人拦在门外。
// 目录价是两位小数,但"基础价 + 逐个 addon 累加"会漂:7.99 + 7.01 在 float64 里
// 是 15.000000000000002。不先舍到分就比的话,"上限 15、总价正好 15"会被自己的
// 浮点误差拦掉,而用户完全无法理解为什么。
func TestMonthlyCapRoundsToCentsBeforeComparing(t *testing.T) {
	// 复刻 PriceForOptions 的算法:基础价起头,逐个 addon 累加。
	// 这三个数都是规规矩矩的两位小数,加完却是 15.000000000000002。
	drifted := 4.07 // 基础机型
	drifted += 2.22 // addon 1
	drifted += 8.71 // addon 2
	if drifted <= 15 {
		t.Fatalf("这组数本该漂到 15 以上,实际 %.17f —— 用例的前提没了,换一组数", drifted)
	}
	stubPricer(t, eur(drifted), nil)
	out, f, _ := runWithCap(t, 15, "EUR")
	if !out.Success {
		t.Fatalf("%.17f 舍到分就是 15.00,不该被拦: %+v", drifted, out)
	}
	if n := f.count("POST /order/cart/" + testCartID + "/checkout"); n != 1 {
		t.Errorf("应当结账一次,实际 %d 次", n)
	}
	f.waitForCall(t, "GET /me/order/"+testOrderID)
}

// Partial = 有 addon 不在目录里,PriceForOptions 自己说了这时给的数字**偏低**。
// 这是最隐蔽的失效方式:它每次都"通过",而且看起来很便宜。必须当不下单处理。
func TestMonthlyCapRefusesPartialPrice(t *testing.T) {
	p := eur(3.00) // 远低于上限 —— 但这个数字本身不可信
	p.Partial = true
	stubPricer(t, p, nil)
	out, f, st := runWithCap(t, 15, "EUR")
	assertRetryLater(t, out, f, st)
}

// 币种对不上不换算,直接不下单。
// 15 EUR 的上限管不了 USD 计价的站点,而猜一个汇率去决定花不花钱是荒唐的。
func TestMonthlyCapRefusesCurrencyMismatch(t *testing.T) {
	stubPricer(t, catalog.PlanPrice{Monthly: 9, Currency: "USD"}, nil) // 数字远低于 15
	out, f, st := runWithCap(t, 15, "EUR")
	assertBlocked(t, out, f, st)
}

// 上限设了却没写币种,同样不下单 —— "15" 不说是哪国的 15 就不是一个上限。
func TestMonthlyCapRefusesMissingCurrency(t *testing.T) {
	stubPricer(t, eur(9), nil)
	out, f, st := runWithCap(t, 15, "")
	assertBlocked(t, out, f, st)
}

// 价格算不出来(目录拉不到 / plan 不在目录里)时不下单。
// "查不到价就先买了再说"正是这道闸存在的理由的反面。
//
// 故意连同错误一起返回一个**看起来完全正常**的报价(9 欧、EUR、非 Partial):
// 只返回零值 PlanPrice 的话,后面的币种检查会顺手把它拦下 ——
// 用例照样绿,但绿的原因不是"err 被处理了"。那样等于没测这条分支。
// (这不是假设:把 err 判断改成 if false 之后,用零值的版本依然通过。)
func TestMonthlyCapRefusesWhenPriceUnknown(t *testing.T) {
	stubPricer(t, eur(9), fmt.Errorf("目录里没有这个 planCode"))
	out, f, st := runWithCap(t, 15, "EUR")
	assertRetryLater(t, out, f, st)
}

// 没设上限的任务(用户自己从界面挑好型号点的单)必须完全不受影响,
// 而且**一次定价查询都不发** —— 补货那一刻的热路径上,多一次目录查询就是多一分慢。
func TestNoCapMeansNoPricingCallAtAll(t *testing.T) {
	calls, _ := stubPricer(t, eur(999), nil) // 真调了的话这个价会把单拦下,立刻暴露
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, availPlainInStock)
	st := newPurchaseTestState(t, f.srv.URL)

	out := PurchaseServer(context.Background(), st, testItem()) // MaxMonthly 保持 0

	if !out.Success {
		t.Fatalf("没设上限的单不该被任何东西拦住: %+v", out)
	}
	if *calls != 0 {
		t.Errorf("没设上限就不该查价,实际查了 %d 次", *calls)
	}
	f.waitForCall(t, "GET /me/order/"+testOrderID)
}

// 拿去算价的必须是**目录里的完整 addon 码**,不是 FQN 里那个短前缀。
//
// 这条最容易写错,而写错的后果恰好是"闸门静默失效":
// availabilities 的 FQN 段是短前缀(ram-64g),eco/options 里是带机型后缀的完整码
// (ram-64g-24sk602)。拿短前缀去 PriceForOptions,目录里查不到 → 标 Partial →
// 按上面那条规则一律不下单。也就是说传错码的表现不是"算错价",
// 而是**每一单都被拦**,用户会以为功能坏了;要是哪天有人为了"修好它"把 Partial
// 那条判断放宽,闸门就真的没了。
func TestMonthlyCapPricesTheAddonsActuallyAddedToCart(t *testing.T) {
	_, gotAddons := stubPricer(t, eur(12), nil)
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	// FQN 第二段是短前缀
	scriptOrderFlow(f, `[{"fqn":"24sk602.ram-64g","planCode":"24sk602",
	  "datacenters":[{"datacenter":"gra","availability":"24H"}]}]`)
	// 车上能选的是带机型后缀的完整码
	f.json("GET /order/cart/"+testCartID+"/eco/options",
		`[{"planCode":"ram-64g-24sk602","prices":[{"duration":"P1M","pricingMode":"default"}]}]`)
	f.json("POST /order/cart/"+testCartID+"/eco/options", `{}`)
	st := newPurchaseTestState(t, f.srv.URL)

	item := testItem() // 不指定 Options → 从 FQN 推,走的正是会出错的那条路
	item.MaxMonthly = 15
	item.MaxMonthlyCurrency = "EUR"

	if out := PurchaseServer(context.Background(), st, item); !out.Success {
		t.Fatalf("12 欧未超 15 欧上限,应当下单: %+v;序列 %v", out, f.seq())
	}
	want := []string{"ram-64g-24sk602"}
	if len(*gotAddons) != 1 || (*gotAddons)[0] != want[0] {
		t.Errorf("算价用的 addon 应当是目录完整码 %v，实际 %v —— "+
			"传 FQN 短前缀会让每一单都被 Partial 拦下", want, *gotAddons)
	}
	f.waitForCall(t, "GET /me/order/"+testOrderID)
}
