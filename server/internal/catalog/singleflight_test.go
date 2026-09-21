package catalog

// loadSubsidiaryCatalog 用的是自己实现的 singleflight:同一子公司同时只允许一次
// 在途拉取,其余调用方等在 <-call.done 上。
//
// 这就意味着**负责拉取的那一方必须保证 done 一定会被关掉**。
// fetchSubsidiaryCatalog 解析的是一份 12MB 的外部 JSON,不能假设它永不 panic;
// 而它一旦 panic,close(call.done) 和 delete(regionCacheCall) 都跑不到 ——
// 之后每一个要这份目录的调用都会永久阻塞,监控、下单的 region 解析、询价
// 全部静默停摆,不重启恢复不了。
//
// 这个用例在没有兜底时会自己挂死在第二次调用上(5 秒后报出来)。

import (
	"testing"
	"time"

	"github.com/ovh-buy/server/internal/app"
)

func TestCatalogFetchPanicDoesNotDeadlockLaterCalls(t *testing.T) {
	resetCatalogCaches()
	orig := fetchSubsidiaryCatalog
	t.Cleanup(func() { fetchSubsidiaryCatalog = orig; resetCatalogCaches() })

	fetchSubsidiaryCatalog = func(_ *app.State, _ string) (*subsidiaryCatalog, error) {
		panic("解析目录时炸了")
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("这一次调用本该把 panic 透出来")
			}
		}()
		_, _ = loadSubsidiaryCatalog(testState(t), "IE")
	}()

	// 关键的是这一次:没有兜底的话它会永远阻塞在 <-call.done 上
	fetchSubsidiaryCatalog = func(_ *app.State, _ string) (*subsidiaryCatalog, error) {
		return &subsidiaryCatalog{
			plans:    map[string]planConfig{"24sk602": {}},
			addons:   map[string]planConfig{},
			currency: "EUR", fetchedAt: time.Now(),
		}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := loadSubsidiaryCatalog(testState(t), "IE"); err != nil {
			t.Errorf("panic 之后应当能正常恢复,实际 %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("panic 之后这个子公司的目录路径被永久卡死了 —— singleflight 的 done 没人关")
	}
}
