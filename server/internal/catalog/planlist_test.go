package catalog

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovh-buy/server/internal/app"
)

func stubCatalogFetch(t *testing.T, cat *subsidiaryCatalog, err error) *int32 {
	t.Helper()
	resetCatalogCaches()
	orig := fetchSubsidiaryCatalog
	var calls int32
	fetchSubsidiaryCatalog = func(_ *app.State, _ string) (*subsidiaryCatalog, error) {
		atomic.AddInt32(&calls, 1)
		if err != nil {
			return nil, err
		}
		c := *cat
		c.fetchedAt = time.Now()
		return &c, nil
	}
	t.Cleanup(func() { fetchSubsidiaryCatalog = orig; resetCatalogCaches() })
	return &calls
}

func twoPlanCatalog() *subsidiaryCatalog {
	return &subsidiaryCatalog{
		plans: map[string]planConfig{
			"24sk602": {
				datacenters: []string{"gra", "rbx"},
				monthly:     PriceParts{Price: 7.00, Tax: 1.40, OK: true},
			},
			"24rise01": {
				datacenters: []string{"bhs"},
				// 目录里没给月费:这种 plan 必须如实报"没价",而不是当成 0 元
				monthly: PriceParts{},
			},
		},
		addons:   map[string]planConfig{},
		currency: "EUR",
	}
}

// 摊平之后要按 planCode 排序(稳定输出),价格含税,缺价如实说缺。
func TestListPlansFlattensCatalog(t *testing.T) {
	stubCatalogFetch(t, twoPlanCatalog(), nil)

	got, err := ListPlans(testState(t), "IE", DefaultMaxAge)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应当有 2 个机型,实际 %d: %+v", len(got), got)
	}
	// 排序:24rise01 < 24sk602
	if got[0].PlanCode != "24rise01" || got[1].PlanCode != "24sk602" {
		t.Errorf("没按 planCode 排序: %s, %s", got[0].PlanCode, got[1].PlanCode)
	}
	// 含税:7.00 + 1.40
	if got[1].Monthly != 8.40 || !got[1].MonthlyOK {
		t.Errorf("月费应当是含税的 8.40,实际 %v (OK=%v)", got[1].Monthly, got[1].MonthlyOK)
	}
	if got[1].Currency != "EUR" {
		t.Errorf("币种应当跟着目录走,实际 %q", got[1].Currency)
	}
	// 缺价的那个:MonthlyOK=false。当成 0 元的话,按价格筛选会把它当成"最便宜的"直接买
	if got[0].MonthlyOK {
		t.Error("目录没给月费的机型必须报 MonthlyOK=false —— 当成 0 元会让价格筛选把它当最便宜的")
	}
	if len(got[1].Datacenters) != 2 {
		t.Errorf("机房列表没带出来: %v", got[1].Datacenters)
	}
}

// 返回的切片被调用方改了,不能污染缓存里那份目录 ——
// 缓存是跨账户共享的,下单链路的 region 解析也吃同一份。
func TestListPlansReturnsCopies(t *testing.T) {
	stubCatalogFetch(t, twoPlanCatalog(), nil)
	first, err := ListPlans(testState(t), "IE", DefaultMaxAge)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	for i := range first {
		for j := range first[i].Datacenters {
			first[i].Datacenters[j] = "污染"
		}
	}
	second, err := ListPlans(testState(t), "IE", DefaultMaxAge)
	if err != nil {
		t.Fatalf("不该出错: %v", err)
	}
	for _, p := range second {
		for _, dc := range p.Datacenters {
			if dc == "污染" {
				t.Fatalf("%s 的机房列表被上一次调用改掉了 —— 返回的是缓存里那份底层数组", p.PlanCode)
			}
		}
	}
}

// AlwaysFresh 必须真的每次都重拉:读缓存的话,最多晚 2 小时才发现新机型,
// 而这条轮询存在的唯一理由就是尽快发现。
func TestAlwaysFreshBypassesCache(t *testing.T) {
	calls := stubCatalogFetch(t, twoPlanCatalog(), nil)

	for i := 0; i < 3; i++ {
		if _, err := ListPlans(testState(t), "IE", AlwaysFresh); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	if n := atomic.LoadInt32(calls); n != 3 {
		t.Errorf("AlwaysFresh 应当每次都真的重拉,实际只拉了 %d 次(读了缓存)", n)
	}

	// 对照:默认新鲜度下三次只该拉一次
	atomic.StoreInt32(calls, 0)
	resetCatalogCaches()
	for i := 0; i < 3; i++ {
		if _, err := ListPlans(testState(t), "IE", DefaultMaxAge); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("默认新鲜度下三次调用只该拉一次目录,实际 %d 次", n)
	}
}

// 强制刷新失败时,必须退回上一次缓存的那份目录,而不是变成"彻底没有目录"。
// 这份缓存是跨账户共享的,下单链路的 region 解析也吃它 ——
// 新机型轮询的一次网络抖动不该把下单也一起弄瘫。
func TestAlwaysFreshFallsBackToStaleOnFailure(t *testing.T) {
	calls := stubCatalogFetch(t, twoPlanCatalog(), nil)
	if _, err := ListPlans(testState(t), "IE", DefaultMaxAge); err != nil {
		t.Fatalf("预热失败: %v", err)
	}
	_ = calls

	// 之后的拉取一律失败
	fetchSubsidiaryCatalog = func(_ *app.State, _ string) (*subsidiaryCatalog, error) {
		return nil, errors.New("网络抖了一下")
	}
	// 注意这里必须给一个带 Logger 的 State:退回旧缓存那条路会写一条 warn,
	// 而 State{} 的 Logger 是 nil —— 空指针 panic 会让 singleflight 的 done
	// channel 永远关不掉,下一个用例直接卡死(这个坑真踩过)。
	got, err := ListPlans(testState(t), "IE", AlwaysFresh)
	if err != nil {
		t.Fatalf("拉取失败时应当退回旧缓存,实际直接报错: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("退回的旧缓存应当还是那 2 个机型,实际 %d 个", len(got))
	}
}

// 拉目录的过程 panic 了,不能把这个子公司的目录路径永久卡死。
//
// 没有兜底的话:panic 跳过了 close(call.done) 和 delete(regionCacheCall),
// 于是之后**每一个**要这份目录的调用都永久阻塞在 <-call.done 上 ——
// 监控、下单的 region 解析、询价全部静默停摆,不重启恢复不了。
// 这个用例自己就会挂死在第二次调用上(靠 go test 的 -timeout 暴露)。
func TestCatalogFetchPanicDoesNotDeadlockLaterCalls(t *testing.T) {
	stubCatalogFetch(t, twoPlanCatalog(), nil)

	fetchSubsidiaryCatalog = func(_ *app.State, _ string) (*subsidiaryCatalog, error) {
		panic("解析目录时炸了")
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("这一次调用本该把 panic 透出来")
			}
		}()
		_, _ = ListPlans(testState(t), "IE", AlwaysFresh)
	}()

	// 关键的是这一次:没有兜底的话它会永远阻塞
	done := make(chan struct{})
	go func() {
		defer close(done)
		fetchSubsidiaryCatalog = func(_ *app.State, _ string) (*subsidiaryCatalog, error) {
			return twoPlanCatalog(), nil
		}
		if _, err := ListPlans(testState(t), "IE", AlwaysFresh); err != nil {
			t.Errorf("panic 之后应当能正常恢复,实际 %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("panic 之后这个子公司的目录路径被永久卡死了 —— singleflight 的 done 没人关")
	}
}
