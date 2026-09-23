package purchase

// 下单成功必须当场落库。
//
// 以前成功只在内存里改成 completed,落库要等整批任务都跑完;同批别的任务
// 卡在 OVH 慢请求上时,这段窗口能有几分钟。其间进程重启,库里这条还是 running,
// 重启后会再下一单 —— 开了自动付款就是重复扣款。
// 这里让同批另一条任务的查库存卡 2 秒,看成功那条在库里是不是已经没了。

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ovh-buy/server/internal/types"
)

func TestSuccessfulOrderLeavesQueueOnDiskImmediately(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, availPlainInStock)
	// 同一批里另一条任务的查库存很慢(补货高峰 OVH 慢/超时很常见)
	f.route("GET /dedicated/server/datacenter/availabilities", func(w http.ResponseWriter, c apiCall) {
		if c.Query == "planCode=slowplan" {
			time.Sleep(2 * time.Second)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[]`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, availPlainInStock)
	})
	st := newPurchaseTestState(t, f.srv.URL)
	// 慢的那条用另一个账户:go-ovh 每次请求都会写一遍共享 http.Client 的 Timeout
	// (同值写,实际无害),两条任务共用一个 client 并发跑会让 -race 报在库里面。
	// 这里要验的是落库时机,不是那个。
	slowAcc := st.Accounts[0]
	slowAcc.ID, slowAcc.IsDefault = "acc-slow", false
	st.Accounts = append(st.Accounts, slowAcc)
	now := types.NowISO()
	fast := types.QueueItem{ID: "task-fast", AccountID: testAccountID, PlanCode: "24sk602", Datacenter: "gra",
		Status: "running", CreatedAt: now, UpdatedAt: now, RetryInterval: 30, AutoPay: true}
	slow := types.QueueItem{ID: "task-slow", AccountID: "acc-slow", PlanCode: "slowplan", Datacenter: "gra",
		Status: "running", CreatedAt: now, UpdatedAt: now, RetryInterval: 30}
	if err := st.EnqueueItems([]types.QueueItem{fast, slow}, false); err != nil {
		t.Fatal(err)
	}

	go ProcessQueueLoop(st)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h, ok := historyFor(t, st, "task-fast"); ok && h.Status == "success" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	h, ok := historyFor(t, st, "task-fast")
	if !ok || h.Status != "success" {
		t.Fatalf("快的那单应该已经下单成功了: %+v", h)
	}
	// 落库在 recordSuccess 之后、同一个 goroutine 里,给它一点时间走完
	time.Sleep(300 * time.Millisecond)
	onDisk, err := st.DB.ListQueue()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range onDisk {
		if it.ID == "task-fast" {
			t.Errorf("订单 %s 已经下成功(autoPay=true),但库里这条任务还在(状态 %q)—— 此刻进程重启,重启后会再下一单",
				h.OrderID, it.Status)
		}
	}
}
