package monitor

// 新机型发现器的用例。
//
// 这条路径会自己决定掏钱买什么(虽然停在未付款),所以每一个"不买"的判据
// 都单独钉一条 —— 少了任何一条,表现都是"它自己买了个我没想买的东西"。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/catalog"
	"github.com/ovh-buy/server/internal/types"
)

// newPlanTestMonitor 复用 renderTestMonitor(带真 SQLite + Logger、走 New() 正常初始化),
// 再把账户表填上 —— 这条路径要按机型所属站点挑账户,没有账户就什么都判断不了。
func newPlanTestMonitor(t *testing.T, accounts []types.OVHAccount) *Monitor {
	t.Helper()
	m := renderTestMonitor(t)
	m.state.Accounts = accounts
	return m
}

func plan(code string, monthly float64, ok bool, currency string) catalog.CatalogPlan {
	return catalog.CatalogPlan{
		PlanCode: code, Monthly: monthly, MonthlyOK: ok,
		Currency: currency, Datacenters: []string{"gra"},
	}
}

func armedConfig() NewPlanWatchConfig {
	return NewPlanWatchConfig{
		Enabled: true, AutoOrder: true,
		MaxMonthly: 15, Currency: "EUR",
		MaxPerRound: 2,
	}.normalized()
}

// 第一次跑必须只建基线、一个都不报。
// 不然首次启动会把目录里三四百个机型全当成"新上架" —— 开了自动下单就是照着买。
func TestFirstRoundOnlyBuildsBaseline(t *testing.T) {
	m := newPlanTestMonitor(t, nil)
	fresh := m.diffNewPlans("IE", []catalog.CatalogPlan{
		plan("24sk602", 8, true, "EUR"),
		plan("24rise01", 12, true, "EUR"),
	})
	if len(fresh) != 0 {
		t.Errorf("首轮应当一个都不报(只建基线)，实际报了 %d 个", len(fresh))
	}
	if m.KnownPlanCount() != 2 {
		t.Errorf("基线应当有 2 个机型，实际 %d", m.KnownPlanCount())
	}
	// 第二轮才开始真的报新增
	fresh = m.diffNewPlans("IE", []catalog.CatalogPlan{
		plan("24sk602", 8, true, "EUR"),
		plan("24rise01", 12, true, "EUR"),
		plan("25new01", 9, true, "EUR"),
	})
	if len(fresh) != 1 || fresh[0].PlanCode != "25new01" {
		t.Errorf("第二轮应当只报新增的 25new01，实际 %+v", fresh)
	}
	// 报过一次就不该再报
	if again := m.diffNewPlans("IE", []catalog.CatalogPlan{plan("25new01", 9, true, "EUR")}); len(again) != 0 {
		t.Errorf("同一个机型不该报第二次，实际 %+v", again)
	}
}

// 每一条"不自动下单"的判据。表驱动:少一条都意味着一种花错钱的方式。
func TestAutoOrderGuards(t *testing.T) {
	cases := []struct {
		name       string
		cfg        func(NewPlanWatchConfig) NewPlanWatchConfig
		p          catalog.CatalogPlan
		already    int
		wantArmed  bool
		wantReason string // 结论里应当出现的关键词
	}{
		{
			name: "价格没超,该买", p: plan("25new01", 9, true, "EUR"),
			wantArmed: true, wantReason: "已自动建监控",
		},
		{
			name: "正好等于上限也该买", p: plan("25new01", 15, true, "EUR"),
			wantArmed: true, wantReason: "已自动建监控",
		},
		{
			name: "超上限不买", p: plan("25new01", 15.01, true, "EUR"),
			wantReason: "超过上限",
		},
		{
			// 目录没给价 ≠ 免费。当成 0 元的话它会"最便宜"从而必定通过上限。
			name: "目录没给月费就不买", p: plan("25new01", 0, false, "EUR"),
			wantReason: "价格未知",
		},
		{
			name: "币种不符不买(不做汇率换算)", p: plan("25new01", 9, true, "USD"),
			wantReason: "不做汇率换算",
		},
		{
			name: "没开自动下单", p: plan("25new01", 9, true, "EUR"),
			cfg:        func(c NewPlanWatchConfig) NewPlanWatchConfig { c.AutoOrder = false; return c },
			wantReason: "未开启自动下单",
		},
		{
			// 没有上限的自动下单 = 一张空白支票
			name: "开了自动下单却没设上限", p: plan("25new01", 9, true, "EUR"),
			cfg:        func(c NewPlanWatchConfig) NewPlanWatchConfig { c.MaxMonthly = 0; return c },
			wantReason: "空白支票",
		},
		{
			name: "上限没写币种", p: plan("25new01", 9, true, "EUR"),
			cfg:        func(c NewPlanWatchConfig) NewPlanWatchConfig { c.Currency = ""; return c },
			wantReason: "币种",
		},
		{
			name: "本轮已到上限", p: plan("25new01", 9, true, "EUR"),
			already: 2, wantReason: "单轮上限",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newPlanTestMonitor(t, []types.OVHAccount{{
				ID: "acc-1", Name: "主号", Endpoint: "ovh-eu", Zone: "IE", IsDefault: true,
			}})
			cfg := armedConfig()
			if tc.cfg != nil {
				cfg = tc.cfg(cfg)
			}

			reason, armed := m.decideNewPlanOrder(cfg, "IE", tc.p, tc.already)

			if armed != tc.wantArmed {
				t.Errorf("该不该建订阅: 期望 %v 实际 %v；结论=%q", tc.wantArmed, armed, reason)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("结论里应当说明 %q，实际 %q", tc.wantReason, reason)
			}
			// 没建订阅就不该在列表里留下任何东西
			if !tc.wantArmed && len(m.subscriptions) != 0 {
				t.Errorf("没决定下单却建了 %d 条订阅", len(m.subscriptions))
			}
		})
	}
}

// 建出来的订阅必须带上月费上限,而且**绝不能带自动付款**。
func TestAutoCreatedSubscriptionNeverAutoPays(t *testing.T) {
	m := newPlanTestMonitor(t, []types.OVHAccount{{
		ID: "acc-1", Name: "主号", Endpoint: "ovh-eu", Zone: "IE", IsDefault: true,
	}})

	if _, armed := m.decideNewPlanOrder(armedConfig(), "IE", plan("25new01", 9, true, "EUR"), 0); !armed {
		t.Fatal("这一条本该建订阅")
	}
	if len(m.subscriptions) != 1 {
		t.Fatalf("应当建出 1 条订阅，实际 %d", len(m.subscriptions))
	}
	s := m.subscriptions[0]
	if s.AutoPay {
		t.Error("自动建的订阅绝不能开自动付款 —— 订单必须停在未付款，由用户自己决定付不付")
	}
	if s.MaxMonthly != 15 || s.MaxMonthlyCurrency != "EUR" {
		t.Errorf("月费上限没带上: %v %q", s.MaxMonthly, s.MaxMonthlyCurrency)
	}
	if !s.AutoOrder || s.Quantity != 1 || s.AutoOrderAccountID != "acc-1" {
		t.Errorf("自动下单配置不对: autoOrder=%v qty=%d acc=%q", s.AutoOrder, s.Quantity, s.AutoOrderAccountID)
	}
	if s.AutoCreatedFrom != "new-plan-watch" {
		t.Errorf("要标明来历，否则用户在监控页看到一条自己没建过的订阅会摸不着头脑: %q", s.AutoCreatedFrom)
	}

	// 同一个机型再来一次不该重复建
	if _, armed := m.decideNewPlanOrder(armedConfig(), "IE", plan("25new01", 9, true, "EUR"), 0); armed {
		t.Error("已经在监控列表里的机型不该重复建订阅")
	}
	if len(m.subscriptions) != 1 {
		t.Errorf("订阅数不该变，实际 %d", len(m.subscriptions))
	}
}

// 指定了下单账户、而那个账户不在这个机型所属的站点时,不能"顺手换一张卡"。
// 用户指定账户往往正是因为只想用那一张卡结账。
func TestNeverSilentlySwapsTheOrderingAccount(t *testing.T) {
	m := newPlanTestMonitor(t, []types.OVHAccount{
		{ID: "acc-eu", Name: "欧区号", Endpoint: "ovh-eu", Zone: "IE", IsDefault: true},
		{ID: "acc-us", Name: "美区号", Endpoint: "ovh-us", Zone: "US"},
	})
	cfg := armedConfig()
	cfg.AccountID = "acc-us" // 用户点名要美区号

	// 却来了个欧区(IE)的新机型
	reason, armed := m.decideNewPlanOrder(cfg, "IE", plan("25new01", 9, true, "EUR"), 0)
	if armed {
		t.Errorf("指定账户不在这个站点，不该改用别的账户下单；结论=%q", reason)
	}
	if !strings.Contains(reason, "账户") {
		t.Errorf("结论里应当说明是账户的问题，实际 %q", reason)
	}
}

// 间隔要夹回合法区间:太密就是拿带宽换几分钟,太稀就失去意义。
func TestClampNewPlanInterval(t *testing.T) {
	if got := ClampNewPlanInterval(1); got != NewPlanMinInterval {
		t.Errorf("1 分钟应当夹到 %d，实际 %d", NewPlanMinInterval, got)
	}
	if got := ClampNewPlanInterval(99999); got != NewPlanMaxInterval {
		t.Errorf("超大值应当夹到 %d，实际 %d", NewPlanMaxInterval, got)
	}
	if got := ClampNewPlanInterval(60); got != 60 {
		t.Errorf("合法值不该被改，实际 %d", got)
	}
	// 单轮上限也要有硬顶:光挡下限的话,界面上填 999 就能一轮建出 999 条自动下单订阅
	if got := (NewPlanWatchConfig{MaxPerRound: 999}).normalized().MaxPerRound; got != NewPlanMaxPerRound {
		t.Errorf("单轮上限 999 应当夹到 %d，实际 %d", NewPlanMaxPerRound, got)
	}
	if got := (NewPlanWatchConfig{MaxPerRound: 3}).normalized().MaxPerRound; got != 3 {
		t.Errorf("合法的单轮上限不该被改，实际 %d", got)
	}

	// 零值配置 = 全关 + 默认间隔
	def := NewPlanWatchConfig{}.normalized()
	if def.Enabled || def.AutoOrder {
		t.Error("默认必须是全关的")
	}
	if def.IntervalMinutes != NewPlanDefaultInterval {
		t.Errorf("默认间隔应当是 %d，实际 %d", NewPlanDefaultInterval, def.IntervalMinutes)
	}
}

// ── 基线必须按子公司各记各的 ──────────────────────────────────────────
//
// 目录是按子公司分的,一轮里逐个拉。拿一个扁平集合判"是不是第一次",
// 第一个子公司建完基线后它就非空了,第二个子公司的**整份目录**会被判成新上架。
// 在"未付款订单会锁住机器"的前提下,这不是多发几条通知的问题 ——
// 开了自动下单就是当场替用户锁住一批他没打算买的机器。
//
// 下面三条是这个 bug 的三条触发路径,各盯一条。

// 路径一:首轮就有多个站点。
func TestBaselineIsPerSubsidiary(t *testing.T) {
	m := newPlanTestMonitor(t, nil)

	if fresh := m.diffNewPlans("IE", []catalog.CatalogPlan{
		plan("24sk602", 8, true, "EUR"),
		plan("24rise01", 12, true, "EUR"),
	}); len(fresh) != 0 {
		t.Fatalf("第一个子公司应当只建基线，实际报了 %d 个", len(fresh))
	}

	fresh := m.diffNewPlans("US", []catalog.CatalogPlan{
		plan("24sk602-us", 8, true, "USD"),
		plan("24rise01-us", 12, true, "USD"),
	})
	if len(fresh) != 0 {
		t.Errorf("同一轮里第二个子公司也该只建基线，实际被当成新上架报了 %v", planCodes(fresh))
	}

	// 两个站点都建过基线之后,各自的新增才开始算数
	if got := m.diffNewPlans("IE", []catalog.CatalogPlan{plan("25new01", 9, true, "EUR")}); len(got) != 1 {
		t.Errorf("IE 建过基线后应当正常报新增，实际 %v", planCodes(got))
	}
	if got := m.diffNewPlans("US", []catalog.CatalogPlan{plan("25new01-us", 9, true, "USD")}); len(got) != 1 {
		t.Errorf("US 建过基线后应当正常报新增，实际 %v", planCodes(got))
	}
}

// 路径二:先只有欧区账户，之后才加了个美区账户。
// 美区第一次被拉到时同样只该建基线。
func TestNewlyAddedRegionOnlyBuildsBaseline(t *testing.T) {
	m := newPlanTestMonitor(t, nil)
	m.diffNewPlans("IE", []catalog.CatalogPlan{plan("24sk602", 8, true, "EUR")})
	// 跑了几轮,欧区一切正常
	m.diffNewPlans("IE", []catalog.CatalogPlan{plan("24sk602", 8, true, "EUR")})

	fresh := m.diffNewPlans("US", []catalog.CatalogPlan{
		plan("24sk602-us", 8, true, "USD"),
		plan("24adv01-us", 40, true, "USD"),
	})
	if len(fresh) != 0 {
		t.Errorf("新加的站点第一次拉到时只该建基线，实际报了 %v", planCodes(fresh))
	}
}

// 路径三:某个站点本轮拉取失败(网络抖动),下一轮才第一次成功。
// 失败那一轮压根没调 diffNewPlans，所以下一轮仍然是它的"第一次"。
func TestSubsidiaryThatFailedEarlierStillGetsBaseline(t *testing.T) {
	m := newPlanTestMonitor(t, nil)
	m.diffNewPlans("IE", []catalog.CatalogPlan{plan("24sk602", 8, true, "EUR")})
	// US 这一轮拉挂了 → runNewPlanRound 里是 continue，不会调 diffNewPlans

	// 下一轮 US 终于拉到了
	fresh := m.diffNewPlans("US", []catalog.CatalogPlan{plan("24sk602-us", 8, true, "USD")})
	if len(fresh) != 0 {
		t.Errorf("上一轮拉失败的站点，这一轮仍是它的第一次，只该建基线，实际报了 %v", planCodes(fresh))
	}
}

// 基线状态要能扛过重启。不然每次重启所有站点都重新建基线,
// 而建基线那一轮不告警 —— 程序没跑的那段时间里上架的新机型会被静默吞掉。
func TestBaselineSurvivesRestart(t *testing.T) {
	m := newPlanTestMonitor(t, nil)
	m.diffNewPlans("IE", []catalog.CatalogPlan{plan("24sk602", 8, true, "EUR")})
	m.SaveToDB()

	// 模拟重启:同一个库,新的 Monitor
	m2 := New(m.state)
	m2.LoadFromDB()

	if got := m2.BaselinedSubsidiaries(); len(got) != 1 || got[0] != "IE" {
		t.Fatalf("基线状态没扛过重启: %v", got)
	}
	// 重启后第一轮就该正常报新增,而不是又白建一次基线
	fresh := m2.diffNewPlans("IE", []catalog.CatalogPlan{
		plan("24sk602", 8, true, "EUR"),
		plan("25new01", 9, true, "EUR"),
	})
	if len(fresh) != 1 || fresh[0].PlanCode != "25new01" {
		t.Errorf("重启后第一轮就该报出新增的 25new01，实际 %v —— "+
			"基线没持久化的话，停机期间上架的机型会被这一轮静默吞掉", planCodes(fresh))
	}
}

func planCodes(ps []catalog.CatalogPlan) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.PlanCode)
	}
	return out
}

// stubListPlans 把目录换成固定内容(按子公司)。
func stubListPlans(t *testing.T, by map[string][]catalog.CatalogPlan) {
	t.Helper()
	orig := listPlans
	listPlans = func(_ *app.State, sub string, _ time.Duration) ([]catalog.CatalogPlan, error) {
		if ps, ok := by[sub]; ok {
			return ps, nil
		}
		return nil, fmt.Errorf("测试里没给 %s 准备目录", sub)
	}
	t.Cleanup(func() { listPlans = orig })
}

// 自动建完订阅,必须保证监控真的在跑。
//
// main.go 只在启动时「已经有订阅才自动启动」监控。于是全新安装、一条订阅都没有的
// 用户打开新机型发现 + 自动下单之后:订阅建出来了,但监控循环根本没起来,
// 那条订阅永远不会被检查 —— 什么都不发生,而且没有任何提示。
//
// 另外这个用例还顺带盯着死锁:Start() 自己要取 subsMu,
// 要是有人把它挪进持锁的 subscribeNewPlan 里,这里会直接挂死(靠 -timeout 暴露)。
func TestNewPlanRoundStartsMonitorAfterCreatingSubscription(t *testing.T) {
	m := newPlanTestMonitor(t, []types.OVHAccount{{
		ID: "acc-1", Name: "主号", Endpoint: "ovh-eu", Zone: "IE", IsDefault: true,
	}})
	t.Cleanup(func() { m.Stop() })

	base := []catalog.CatalogPlan{plan("24sk602", 8, true, "EUR")}
	stubListPlans(t, map[string][]catalog.CatalogPlan{"IE": base})
	cfg := armedConfig()

	// 第一轮:只建基线,不该建订阅,也不该因此启动监控
	genBefore := monitorGeneration(m)
	m.runNewPlanRound(cfg)
	if len(m.Snapshot()) != 0 {
		t.Fatalf("首轮只建基线，不该建订阅，实际建了 %d 条", len(m.Snapshot()))
	}
	if monitorGeneration(m) != genBefore {
		t.Error("首轮什么都没建，不该顺带启动监控")
	}

	// 第二轮:冒出一个 9 欧的新机型 → 建订阅 → 监控必须被拉起来
	stubListPlans(t, map[string][]catalog.CatalogPlan{
		"IE": append(append([]catalog.CatalogPlan{}, base...), plan("25new01", 9, true, "EUR")),
	})
	m.runNewPlanRound(cfg)

	subs := m.Snapshot()
	if len(subs) != 1 || subs[0].PlanCode != "25new01" {
		t.Fatalf("应当为 25new01 建一条订阅，实际 %+v", subs)
	}
	// 断言代际号涨了,而不是断言 running 为真:监控循环起来之后会先查通知通道,
	// 测试环境里一条都没配,它会按设计自停(checkNotifyOrStop)——
	// 那是另一件事,不该让这个用例跟着它抖。代际号只在 Start() 里递增,
	// 所以它涨了就等于 Start() 真的被调过。
	if monitorGeneration(m) == genBefore {
		t.Error("建了自动下单订阅却没启动监控 —— 那条订阅永远不会被检查，" +
			"用户看到的是「开了自动买但什么都没发生」")
	}
}

// 单轮上限要管住整轮,而不是每个站点各算一遍。
func TestMaxPerRoundAppliesAcrossSubsidiaries(t *testing.T) {
	m := newPlanTestMonitor(t, []types.OVHAccount{
		{ID: "acc-eu", Name: "欧区号", Endpoint: "ovh-eu", Zone: "IE", IsDefault: true},
		{ID: "acc-us", Name: "美区号", Endpoint: "ovh-us", Zone: "US"},
	})
	t.Cleanup(func() { m.Stop() })

	euBase := []catalog.CatalogPlan{plan("24sk602", 8, true, "EUR")}
	usBase := []catalog.CatalogPlan{plan("24sk602-us", 8, true, "USD")}
	stubListPlans(t, map[string][]catalog.CatalogPlan{"IE": euBase, "US": usBase})

	cfg := armedConfig()
	cfg.MaxPerRound = 1
	m.runNewPlanRound(cfg) // 建基线

	// 两个站点各冒出一个便宜的新机型,但单轮上限是 1
	stubListPlans(t, map[string][]catalog.CatalogPlan{
		"IE": append(append([]catalog.CatalogPlan{}, euBase...), plan("25new-eu", 9, true, "EUR")),
		"US": append(append([]catalog.CatalogPlan{}, usBase...), plan("25new-us", 9, true, "USD")),
	})
	m.runNewPlanRound(cfg)

	if n := len(m.Snapshot()); n != 1 {
		t.Errorf("单轮上限 1 应当管住整轮（不是每个站点各来一次），实际建了 %d 条", n)
	}
}

// monitorGeneration 带锁读一次代际号。它只在 Start() 里递增,
// 所以"涨没涨"就是"Start 有没有被调过",不受循环自停的影响。
// 包内测试专用,不为此往生产代码加导出方法。
func monitorGeneration(m *Monitor) int64 {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	return m.generation
}

// 等待要切片,让配置改动很快生效。
// 一觉睡满的话,用户把间隔设成 24 小时后想改回 15 分钟得等满一天。
func TestSleepInSlicesWakesEarlyWhenConfigChanges(t *testing.T) {
	orig := newPlanSleepSlice
	newPlanSleepSlice = 10 * time.Millisecond
	t.Cleanup(func() { newPlanSleepSlice = orig })

	start := time.Now()
	n := 0
	// 名义上等 10 分钟,但第一片之后就说"配置变了"
	sleepInSlices(10*time.Minute, func() bool {
		n++
		return true
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("配置一变就该提前醒来，实际等了 %v", elapsed)
	}
	if n != 1 {
		t.Errorf("应当只问一次就醒，实际问了 %d 次", n)
	}
}

// 配置没变就得老老实实睡满(用一小段验证它不会空转)。
func TestSleepInSlicesSleepsWhenNothingChanges(t *testing.T) {
	start := time.Now()
	sleepInSlices(50*time.Millisecond, func() bool { return false })
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("配置没变时不该提前返回，实际只等了 %v", elapsed)
	}
}
