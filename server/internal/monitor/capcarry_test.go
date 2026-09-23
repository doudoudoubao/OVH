package monitor

// 订阅上的配置要经过四次拷贝才能变成一次真实下单:
//
//	Subscription --checkConfig()--> subCheckConfig --batchOrder--> HTTP payload
//	Subscription --toDBSub()--> types.Subscription --fromDBSub()--> Subscription
//	Subscription --snapshot()--> Subscription
//
// 每一处都是手写的字段罗列。漏抄一个字段编译器不会说话,而漏抄**月费上限**的后果是
// 闸门静默失效 —— 新机型自动下单会变回无上限,一台 180 欧/月的机器照买不误。
//
// 所以这里用反射兜底:拿一个"每个字段都非零"的订阅走一遍,任何一个字段在对面是零值
// 就说明拷贝时漏了它。这样以后往订阅上加字段,忘记抄的那一刻就会变红。

import (
	"reflect"
	"testing"

	"github.com/ovh-buy/server/internal/types"
)

// fullySetSubscription 每个会参与下单决策的字段都填上非零值。
// 特意不用零值:零值在对面也是零值,漏抄看不出来。
func fullySetSubscription() *Subscription {
	return &Subscription{
		PlanCode:           "24sk602",
		Datacenters:        []string{"gra", "rbx"},
		NotifyAvailable:    true,
		NotifyUnavailable:  true,
		LastStatus:         map[string]string{"gra": "24H"},
		CreatedAt:          "2026-09-21T00:00:00.000000",
		ServerName:         "测试机型",
		AutoOrder:          true,
		Quantity:           2,
		AutoOrderAccountID: "acc-1",
		AutoPay:            true,
		Options:            []string{"ram-64g"},
		MaxMonthly:         15,
		MaxMonthlyCurrency: "EUR",
		AutoCreatedFrom:    "new-plan-watch",
	}
}

// checkConfig 是每轮检查读配置的唯一入口,batchOrder 的每个参数都来自它。
// 它漏抄哪个字段,那个字段在自动下单时就等于没设置过。
func TestCheckConfigCarriesEveryField(t *testing.T) {
	got := fullySetSubscription().checkConfig()

	v := reflect.ValueOf(got)
	ty := v.Type()
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("checkConfig() 没把 %s 抄过来(拿到零值)——"+
				"这个字段在自动下单时等于没设置过", ty.Field(i).Name)
		}
	}
}

// 落库再读回来之后,上限必须还在。
// 不在的话:重启一次,所有自动建的订阅就都变成"无上限自动下单"了。
func TestMonthlyCapSurvivesDBRoundTrip(t *testing.T) {
	orig := fullySetSubscription()
	back := fromDBSub(toDBSub(orig))

	if back.MaxMonthly != orig.MaxMonthly {
		t.Errorf("月费上限没扛过落库往返: %v → %v（重启后就成了无上限自动下单）",
			orig.MaxMonthly, back.MaxMonthly)
	}
	if back.MaxMonthlyCurrency != orig.MaxMonthlyCurrency {
		t.Errorf("上限币种没扛过落库往返: %q → %q（币种丢了会被闸门一律拒绝下单）",
			orig.MaxMonthlyCurrency, back.MaxMonthlyCurrency)
	}
	if back.AutoCreatedFrom != orig.AutoCreatedFrom {
		t.Errorf("订阅来历没扛过落库往返: %q → %q", orig.AutoCreatedFrom, back.AutoCreatedFrom)
	}
	// 顺手确认这一趟没把别的关键字段弄丢
	if back.AutoOrder != orig.AutoOrder || back.Quantity != orig.Quantity ||
		back.AutoOrderAccountID != orig.AutoOrderAccountID || back.AutoPay != orig.AutoPay {
		t.Errorf("自动下单相关字段没扛过落库往返: %+v", back)
	}
}

// snapshot() 是通知渲染那条路读订阅的入口,同样不能把上限弄丢。
func TestSnapshotCarriesMonthlyCap(t *testing.T) {
	orig := fullySetSubscription()
	snap := orig.snapshot()
	if snap.MaxMonthly != orig.MaxMonthly || snap.MaxMonthlyCurrency != orig.MaxMonthlyCurrency {
		t.Errorf("snapshot 丢了月费上限: %v %q → %v %q",
			orig.MaxMonthly, orig.MaxMonthlyCurrency, snap.MaxMonthly, snap.MaxMonthlyCurrency)
	}
	if snap.AutoCreatedFrom != orig.AutoCreatedFrom {
		t.Errorf("snapshot 丢了订阅来历: %q → %q", orig.AutoCreatedFrom, snap.AutoCreatedFrom)
	}
}

// types.Subscription 是落库用的形状,确认新字段真的在上面(而不是只加在内存结构里)。
func TestDBShapeHasCapFields(t *testing.T) {
	var s types.Subscription
	ty := reflect.TypeOf(s)
	for _, name := range []string{"MaxMonthly", "MaxMonthlyCurrency", "AutoCreatedFrom"} {
		if _, ok := ty.FieldByName(name); !ok {
			t.Errorf("types.Subscription 缺字段 %s —— 只加在内存结构里的话,重启就丢", name)
		}
	}
}

// 上面那条只测了两个结构体之间的转换,**没经过数据库** —— 当时表里根本没有上限的列,
// 它照样是绿的,而真实重启后自动建的订阅已经变成"自动下单 + 不限价"。
// 这条走真 SQLite:建订阅 → SaveToDB → 新的 Monitor 从同一个库 LoadFromDB。
// LoadFromDB 不只在启动时跑,代理看门狗跳闸时也会整表重载,所以这里等于两条路一起盯。
func TestMonthlyCapSurvivesRestartThroughSQLite(t *testing.T) {
	m := newPlanTestMonitor(t, []types.OVHAccount{{
		ID: "acc-1", Name: "主号", Endpoint: "ovh-eu", Zone: "IE", IsDefault: true,
	}})
	acc, _ := m.state.FindAccount("acc-1")
	if !m.subscribeNewPlan(armedConfig(), plan("25new01", 9, true, "EUR"), acc) {
		t.Fatal("没建出订阅")
	}
	m.SaveToDB()

	restarted := New(m.state)
	restarted.LoadFromDB()
	subs := restarted.Snapshot()
	if len(subs) != 1 {
		t.Fatalf("重启后应有 1 条订阅,实际 %d", len(subs))
	}
	s := subs[0]
	if !s.AutoOrder {
		t.Fatal("前提不成立:重启后自动下单应当还开着")
	}
	if s.MaxMonthly != 15 || s.MaxMonthlyCurrency != "EUR" {
		t.Errorf("重启后月费上限变成 %v %q —— 这条订阅成了不限价自动下单", s.MaxMonthly, s.MaxMonthlyCurrency)
	}
	if s.AutoCreatedFrom != "new-plan-watch" {
		t.Errorf("重启后订阅来历丢了: %q", s.AutoCreatedFrom)
	}
}
