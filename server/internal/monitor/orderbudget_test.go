package monitor

// 一次补货到底会下几单。
//
// 监控按配置(FQN)逐套触发下单,而封顶以前是按单次 batchOrder 截的 ——
// 等于"每套配置最多 10 单"。盯全部配置的订阅,4 套配置 × 4 个机房同时有货,
// 一次检查就发出 16 个下单请求;新机型自动建的订阅(quantity 1)同样是 16 单,
// 每单都单独通过月费闸。这里用假 OVH + 假本机 API 把整轮检查真跑一遍,数下单请求。

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/config"
	"github.com/ovh-buy/server/internal/db"
	"github.com/ovh-buy/server/internal/logger"
	"github.com/ovh-buy/server/internal/netfp"
	"github.com/ovh-buy/server/internal/storage"
	"github.com/ovh-buy/server/internal/types"
)

// runOneCheckCountingOrders 对给定订阅真跑一轮 CheckAvailabilityChange,返回发出的下单请求数。
func runOneCheckCountingOrders(t *testing.T, sub *Subscription) (int, []string) {
	t.Helper()
	if err := netfp.SetSharedProxy("http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netfp.SetSharedProxy("") })

	// 假 OVH:一个机型,4 套配置(FQN),每套在 4 个机房都有货
	fqns := []string{"25new01.ram-32g.softraid-2x1000", "25new01.ram-64g.softraid-2x1000", "25new01.ram-64g.softraid-2x2000", "25new01.ram-32g.softraid-2x2000"}
	ovhSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/time":
			fmt.Fprintf(w, "%d", time.Now().Unix())
		case "/dedicated/server/datacenter/availabilities":
			out := []map[string]interface{}{}
			for _, f := range fqns {
				out = append(out, map[string]interface{}{
					"fqn": f, "planCode": "25new01", "memory": "ram", "storage": "disk",
					"datacenters": []map[string]string{
						{"datacenter": "gra", "availability": "1H-low"},
						{"datacenter": "rbx", "availability": "1H-low"},
						{"datacenter": "sbg", "availability": "72H"}, {"datacenter": "bhs", "availability": "24H"},
					},
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"message":"nope"}`)
		}
	}))
	defer ovhSrv.Close()

	// 假的本机 API:验价一律通过,quick-order 计数
	var quick int32
	var mu sync.Mutex
	seen := []string{}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/internal/monitor/price":
			_, _ = io.WriteString(w, `{"success":true,"price":{"prices":{"withTax":9.99,"currencyCode":"EUR"}}}`)
		case "/api/queue/quick-order":
			atomic.AddInt32(&quick, 1)
			var b map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			seen = append(seen, fmt.Sprintf("%v@%v", b["planCode"], b["datacenter"]))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer local.Close()
	_, port, _ := net.SplitHostPort(local.Listener.Addr().String())

	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	lg := logger.New(filepath.Join(dir, "t.log"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	st := app.NewState(storage.Paths{DataDir: dir}, config.New(database), lg, database)
	st.Port = port
	st.Accounts = []types.OVHAccount{{ID: "acc-1", Name: "主号", Endpoint: ovhSrv.URL, Zone: "IE",
		AppKey: "ak", AppSecret: "as", ConsumerKey: "ck", IsDefault: true}}
	m := New(st)

	sub.PlanCode = "25new01"
	sub.Datacenters = []string{}
	sub.LastStatus = map[string]string{}
	sub.History = []HistoryEntry{}
	sub.AutoOrder = true
	sub.AutoOrderAccountID = "acc-1"
	m.subscriptions = append(m.subscriptions, sub)

	m.CheckAvailabilityChange(sub, "trace-1")

	return int(atomic.LoadInt32(&quick)), seen
}

// 新机型自动建的订阅:quantity 1 = 一次补货只抢一台,不管同时上了几套配置、几个机房。
func TestNewPlanSubscriptionOrdersOnlyQuantityPerTrigger(t *testing.T) {
	got, seen := runOneCheckCountingOrders(t, &Subscription{
		NotifyAvailable: true, Quantity: 1,
		MaxMonthly: 15, MaxMonthlyCurrency: "EUR", AutoCreatedFrom: "new-plan-watch",
	})
	if got != 1 {
		t.Errorf("新机型订阅 quantity=1,4 套配置 × 4 个机房同时有货时应只下 1 单,实际 %d 单: %v", got, seen)
	}
}

// 用户自己建的、盯全部配置的订阅:按配置逐套下单是既有语义,
// 但"一次检查最多 maxOrdersPerTrigger 单"必须跨配置累计,不能每套配置各算一遍。
func TestOrderCapSpansAllConfigsOfOneCheck(t *testing.T) {
	got, seen := runOneCheckCountingOrders(t, &Subscription{NotifyAvailable: true, Quantity: 1})
	if got != maxOrdersPerTrigger {
		t.Errorf("4 套配置 × 4 个机房 = 16 个目标,一次检查应封顶在 %d 单,实际 %d 单: %v",
			maxOrdersPerTrigger, got, seen)
	}
}
