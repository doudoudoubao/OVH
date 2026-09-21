package monitor

// 新机型发现器的用例。
//
// 这条路径会自己决定掏钱买什么(虽然停在未付款),所以每一个"不买"的判据
// 都单独钉一条 —— 少了任何一条,表现都是"它自己买了个我没想买的东西"。

import (
	"strings"
	"testing"

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
	fresh := m.diffNewPlans([]catalog.CatalogPlan{
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
	fresh = m.diffNewPlans([]catalog.CatalogPlan{
		plan("24sk602", 8, true, "EUR"),
		plan("24rise01", 12, true, "EUR"),
		plan("25new01", 9, true, "EUR"),
	})
	if len(fresh) != 1 || fresh[0].PlanCode != "25new01" {
		t.Errorf("第二轮应当只报新增的 25new01，实际 %+v", fresh)
	}
	// 报过一次就不该再报
	if again := m.diffNewPlans([]catalog.CatalogPlan{plan("25new01", 9, true, "EUR")}); len(again) != 0 {
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
