package purchase

// PurchaseServer 是整个项目里唯一真正花钱的函数,五百多行、十来个分支,
// 在此之前**一行都没被测过**(包覆盖率 12%,全在 timing/transient 这些小工具上)。
//
// 它没被测的原因看着像"它需要一个真的 OVH client",但其实不需要:
// go-ovh 的 loadConfig 写得很清楚 —— "If endpoint contains a '/', consider it as a URL"。
// 也就是说账户表里的 Endpoint 字段本身就是现成的注入点:填 httptest 服务器的地址,
// 整条链路(签名、JSON 编解码、netfp 出站、go-ovh 的错误包装)全都是真的在跑,
// 只有 OVH 那一端是假的。不需要给生产代码加任何 test-only 的接缝。
//
// 这里盯的是四类「错了就要花钱或丢单」的行为:
//   1. 调用顺序(assign 必须在 eco 之前)和三个 configuration 的取值
//   2. checkout 的 body —— autoPay 跟着用户开关走,waiveRetractationPeriod 一个字都不发
//   3. 失败路径不留僵尸 cart,429 不把任务判死
//   4. 取消在结账前必须拦住,结账一旦发出就绝不能中途放手

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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

const (
	testAccountID = "acc-1"
	testCartID    = "cart-1"
	testItemID    = 424242
	testOrderID   = "9999"
)

// ── 假的 OVH ──────────────────────────────────────────────────────────────

type apiCall struct {
	Method string
	Path   string // 不含 query
	Query  string
	Body   map[string]interface{}
}

func (c apiCall) key() string { return c.Method + " " + c.Path }

type fakeOVH struct {
	srv *httptest.Server

	mu     sync.Mutex
	calls  []apiCall
	routes map[string]func(w http.ResponseWriter, c apiCall)
}

func newFakeOVH(t *testing.T) *fakeOVH {
	t.Helper()
	f := &fakeOVH{routes: map[string]func(http.ResponseWriter, apiCall){}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	// go-ovh 每建一个 client,第一次需要签名的调用之前都会先无鉴权地问一次
	// /auth/time 算本机与 OVH 的时差(结果缓存在 client 上,只问一次)。
	f.route("GET /auth/time", func(w http.ResponseWriter, _ apiCall) {
		fmt.Fprintf(w, "%d", time.Now().Unix())
	})
	return f
}

func (f *fakeOVH) route(key string, fn func(w http.ResponseWriter, c apiCall)) {
	f.mu.Lock()
	f.routes[key] = fn
	f.mu.Unlock()
}

// json 注册一条固定返回 200 + 该 JSON 的路由。
func (f *fakeOVH) json(key, body string) {
	f.route(key, func(w http.ResponseWriter, _ apiCall) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
}

// fail 注册一条固定返回指定状态码的路由(OVH 的错误体格式)。
func (f *fakeOVH) fail(key string, status int, msg string) {
	f.route(key, func(w http.ResponseWriter, _ apiCall) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	})
}

func (f *fakeOVH) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	c := apiCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
	if len(raw) > 0 {
		// 解不动就留 nil:assign 那种无 body 的调用本来就没东西可解
		_ = json.Unmarshal(raw, &c.Body)
	}

	f.mu.Lock()
	f.calls = append(f.calls, c)
	fn := f.routes[c.key()]
	f.mu.Unlock()

	if fn == nil {
		// 没登记的路径一律 404 并记下来 —— 测试里断言调用序列时会暴露出来,
		// 比默默返回 200 让用例"碰巧通过"强。
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "测试里没有为 " + c.key() + " 登记响应",
		})
		return
	}
	fn(w, c)
}

func (f *fakeOVH) snapshot() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]apiCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// seq 调用序列,形如 "POST /order/cart"。/auth/time 是 go-ovh 的内务,不算业务步骤。
func (f *fakeOVH) seq() []string {
	out := []string{}
	for _, c := range f.snapshot() {
		if c.Path == "/auth/time" {
			continue
		}
		out = append(out, c.key())
	}
	return out
}

func (f *fakeOVH) count(key string) int {
	n := 0
	for _, c := range f.snapshot() {
		if c.key() == key {
			n++
		}
	}
	return n
}

// bodyOf 取某条调用的请求体;没调用过返回 nil, false。
func (f *fakeOVH) bodyOf(key string) (map[string]interface{}, bool) {
	for _, c := range f.snapshot() {
		if c.key() == key {
			return c.Body, true
		}
	}
	return nil, false
}

// bodiesOf 取某条调用的全部请求体(configuration 会被打三次)。
func (f *fakeOVH) bodiesOf(key string) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, c := range f.snapshot() {
		if c.key() == key {
			out = append(out, c.Body)
		}
	}
	return out
}

// waitForCall 等某条调用出现(异步补单那种在 goroutine 里发的)。
func (f *fakeOVH) waitForCall(t *testing.T, key string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(key) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("等了 3 秒也没等到 %s", key)
}

// ── 测试用的 State ────────────────────────────────────────────────────────

// newPurchaseTestState 真的开一个临时 SQLite,因为 recordSuccess / recordFailure
// 会 go state.SaveHistory() —— 给个 nil DB 只会把问题推到别处。
// endpoint 为空表示"一个账户都没有"。
func newPurchaseTestState(t *testing.T, endpoint string) *app.State {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	lg := logger.New(filepath.Join(dir, "t.log"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	st := app.NewState(storage.Paths{DataDir: dir}, config.New(database), lg, database)
	if endpoint != "" {
		st.Accounts = []types.OVHAccount{{
			ID: testAccountID, Name: "测试号",
			// go-ovh: "If endpoint contains a '/', consider it as a URL"
			Endpoint:  endpoint,
			Zone:      "IE",
			AppKey:    "ak",
			AppSecret: "as", ConsumerKey: "ck",
			IsDefault: true,
		}}
	}
	return st
}

// isolatePublicCatalog 掐断公开目录那条路。
//
// catalog.ResolveRegion 会先去拉公开 eco 目录,而那条请求走的是 netfp.Shared +
// 硬编码的 eu.api.ovh.com,不归 fakeOVH 管。不掐掉的话这些单元测试会真的去打 OVH:
// 在 CI 上就是每次 push 都往 OVH 的限流里送,而限流撞上了就是抢购窗口里查不到库存。
//
// 指向一个不可能有人监听的端口 = 连接立刻被拒(不是等超时),
// ResolveRegion 于是确定性地走静态兜底 gra + IE → europe。
func isolatePublicCatalog(t *testing.T) {
	t.Helper()
	if err := netfp.SetSharedProxy("http://127.0.0.1:1"); err != nil {
		t.Fatalf("配置测试用出口失败: %v", err)
	}
	t.Cleanup(func() { _ = netfp.SetSharedProxy("") })
}

func historyFor(t *testing.T, st *app.State, taskID string) (types.PurchaseHistoryEntry, bool) {
	t.Helper()
	st.HistoryMu.Lock()
	defer st.HistoryMu.Unlock()
	for _, h := range st.History {
		if h.TaskID == taskID {
			return h, true
		}
	}
	return types.PurchaseHistoryEntry{}, false
}

func testItem() *types.QueueItem {
	return &types.QueueItem{
		ID:         "task-1",
		AccountID:  testAccountID,
		PlanCode:   "24sk602",
		Datacenter: "gra",
	}
}

// scriptOrderFlow 铺好一条能走通的下单链路。
// availJSON 交给调用方是因为有货/无货/FQN 覆盖不覆盖所选硬件,正是各用例要拨的那根弦。
func scriptOrderFlow(f *fakeOVH, availJSON string) {
	f.json("GET /dedicated/server/datacenter/availabilities", availJSON)
	f.json("POST /order/cart", `{"cartId":"`+testCartID+`"}`)
	f.json("POST /order/cart/"+testCartID+"/assign", `{}`)
	f.json("POST /order/cart/"+testCartID+"/eco", fmt.Sprintf(`{"itemId":%d}`, testItemID))
	f.json(fmt.Sprintf("POST /order/cart/%s/item/%d/configuration", testCartID, testItemID), `{}`)
	f.json("DELETE /order/cart/"+testCartID, `{}`)
	f.json("POST /order/cart/"+testCartID+"/checkout",
		`{"orderId":`+testOrderID+`,"url":"https://example.invalid/order/`+testOrderID+`"}`)
	f.json("GET /me/order/"+testOrderID, `{"expirationDate":"2030-01-01T00:00:00+01:00"}`)
}

// 有货、且这条 FQN 没有 addon 段(第一段就是全部)——
// 用户没指定硬件选项时会走"不加任何 addon"的最短路径。
const availPlainInStock = `[{"fqn":"24sk602","planCode":"24sk602",
  "datacenters":[{"datacenter":"gra","availability":"24H"}]}]`

// ── 用例 ──────────────────────────────────────────────────────────────────

// 账户被删 / 密钥没填全 = 重试一万次也不会变的错。
// 以前这里连 history 都不写,任务就在后台每 retryInterval 秒静默失败一次,
// 用户只看到任务一直"运行中"却什么都不发生。
func TestPurchaseServerMissingAccountIsFatalAndRecorded(t *testing.T) {
	st := newPurchaseTestState(t, "") // 一个账户都没有
	item := testItem()

	out := PurchaseServer(context.Background(), st, item)

	if !out.Fatal {
		t.Errorf("账户不可用必须判 Fatal(否则按 retryInterval 无限重试),实际 %+v", out)
	}
	if out.Success {
		t.Error("不该报成功")
	}
	h, ok := historyFor(t, st, item.ID)
	if !ok {
		t.Fatal("必须留一条失败记录,否则用户在「购买历史」里什么都看不到")
	}
	if h.Status != "failed" {
		t.Errorf("history 状态应为 failed,实际 %q", h.Status)
	}
}

// 走通一整单:调用顺序、三个必需配置的取值、checkout 的 body 一次钉死。
func TestPurchaseServerHappyPathSequenceAndCheckoutPayload(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, availPlainInStock)
	st := newPurchaseTestState(t, f.srv.URL)
	item := testItem()

	out := PurchaseServer(context.Background(), st, item)

	if !out.Success {
		t.Fatalf("应当下单成功,实际 %+v;调用序列 %v", out, f.seq())
	}

	cfgKey := fmt.Sprintf("POST /order/cart/%s/item/%d/configuration", testCartID, testItemID)
	want := []string{
		"GET /dedicated/server/datacenter/availabilities",
		"POST /order/cart",
		// assign 必须在 eco 之前:对齐 OVH 官方示例的 cart → assign → eco 顺序,
		// 反过来会撞上"cart 未绑定就 checkout"的边界错误。
		"POST /order/cart/" + testCartID + "/assign",
		"POST /order/cart/" + testCartID + "/eco",
		cfgKey, cfgKey, cfgKey,
		"POST /order/cart/" + testCartID + "/checkout",
	}
	got := f.seq()
	if len(got) < len(want) {
		t.Fatalf("调用序列比预期短\n实际 %v\n预期 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 步应当是 %s,实际 %s\n完整序列 %v", i+1, want[i], got[i], got)
		}
	}

	// 三个必需配置严格串行,顺序固定:机房 → 系统 → region。
	// region 的合法取值依赖机房,顺序反了 OVH 侧的校验结果就不确定。
	cfgs := f.bodiesOf(cfgKey)
	wantCfg := []struct{ label, value string }{
		{"dedicated_datacenter", "gra"},
		{"dedicated_os", "none_64.en"},
		// 公开目录被掐断 → 静态兜底 (gra, IE) → europe。
		// 这个值错了(老实现发过 "usa"/"apac")就是每一单都卡在这一步。
		{"region", "europe"},
	}
	if len(cfgs) != len(wantCfg) {
		t.Fatalf("应当发 %d 条 configuration,实际 %d 条: %v", len(wantCfg), len(cfgs), cfgs)
	}
	for i, w := range wantCfg {
		if cfgs[i]["label"] != w.label || cfgs[i]["value"] != w.value {
			t.Errorf("第 %d 条配置应当是 %s=%s,实际 %v", i+1, w.label, w.value, cfgs[i])
		}
	}

	// checkout 的 body 是整条链路里唯一花钱的输入,逐字段钉死。
	body, ok := f.bodyOf("POST /order/cart/" + testCartID + "/checkout")
	if !ok {
		t.Fatal("没发出 checkout")
	}
	if v, has := body["autoPayWithPreferredPaymentMethod"]; !has || v != false {
		t.Errorf("任务没开自动付款时必须显式发 false,实际 %v (has=%v)", v, has)
	}
	// waiveRetractationPeriod = 放弃 14 天无理由撤回权。一旦发 true,权利当场消失,
	// 没有任何接口能拿回来;不发的代价是零(checkout 只是建订单,卡点从来不是它)。
	if _, has := body["waiveRetractationPeriod"]; has {
		t.Errorf("绝不能替用户放弃撤回权,body 里不该出现 waiveRetractationPeriod: %v", body)
	}

	// 成功 = cart 已转 order,不该再去删它(删了是 404,纯噪音)
	if n := f.count("DELETE /order/cart/" + testCartID); n != 0 {
		t.Errorf("下单成功后不该删 cart,实际删了 %d 次", n)
	}

	h, ok := historyFor(t, st, item.ID)
	if !ok || h.Status != "success" || h.OrderID != testOrderID {
		t.Errorf("history 应当是 success + 订单号 %s,实际 %+v (found=%v)", testOrderID, h, ok)
	}

	// 价格/过期时间是下单后异步补的,等它落地顺便确认这个 goroutine 真的发了请求
	f.waitForCall(t, "GET /me/order/"+testOrderID)
}

// 自动付款是"直接从用户账户扣钱"的开关,必须一比一跟着任务上的设置走,
// 既不能默认打开,也不能用户开了却没传过去。
func TestPurchaseServerAutoPayFollowsTaskSetting(t *testing.T) {
	for _, autoPay := range []bool{false, true} {
		t.Run(fmt.Sprintf("autoPay=%v", autoPay), func(t *testing.T) {
			isolatePublicCatalog(t)
			f := newFakeOVH(t)
			scriptOrderFlow(f, availPlainInStock)
			st := newPurchaseTestState(t, f.srv.URL)
			item := testItem()
			item.AutoPay = autoPay

			if out := PurchaseServer(context.Background(), st, item); !out.Success {
				t.Fatalf("应当下单成功,实际 %+v", out)
			}
			body, ok := f.bodyOf("POST /order/cart/" + testCartID + "/checkout")
			if !ok {
				t.Fatal("没发出 checkout")
			}
			if body["autoPayWithPreferredPaymentMethod"] != autoPay {
				t.Errorf("任务设的是 %v,发出去的是 %v", autoPay, body["autoPayWithPreferredPaymentMethod"])
			}
			f.waitForCall(t, "GET /me/order/"+testOrderID)
		})
	}
}

// checkout 失败时:必须清掉 OVH 侧的 cart(高频抢购累计能堆上千个僵尸 cart,
// 进而把自己打进限流),而且 429 不能被当成"一次失败尝试"。
func TestPurchaseServerCheckoutFailureCleansCartAndKeepsRetrying(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		msg           string
		wantAttempted bool
	}{
		{
			// 补货那一瞬间所有人都在打同一个接口,429 是常态。
			// 算成一次失败尝试的话,MaxRetries=5 的任务不到一分钟就把自己判死 ——
			// 而那一分钟正是唯一有货的窗口。
			name: "429 限流不计失败次数", status: 429, msg: "too many requests", wantAttempted: false,
		},
		{
			// 业务级 4xx 重试多少次都是同一个答案,该计数就得计数。
			name: "400 业务拒绝要计失败次数", status: 400, msg: "invalid cart", wantAttempted: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePublicCatalog(t)
			f := newFakeOVH(t)
			scriptOrderFlow(f, availPlainInStock)
			f.fail("POST /order/cart/"+testCartID+"/checkout", tc.status, tc.msg)
			st := newPurchaseTestState(t, f.srv.URL)
			item := testItem()

			out := PurchaseServer(context.Background(), st, item)

			if out.Success {
				t.Fatal("checkout 失败却报了成功")
			}
			if out.Fatal {
				t.Errorf("checkout 的 %d 不是确定性失败,不该把任务判死: %+v", tc.status, out)
			}
			if out.Attempted != tc.wantAttempted {
				t.Errorf("Attempted 应为 %v,实际 %v", tc.wantAttempted, out.Attempted)
			}
			if n := f.count("DELETE /order/cart/" + testCartID); n != 1 {
				t.Errorf("失败后必须清掉 cart,实际 DELETE 了 %d 次;序列 %v", n, f.seq())
			}
			if h, ok := historyFor(t, st, item.ID); !ok || h.Status != "failed" {
				t.Errorf("history 应当留一条 failed,实际 %+v (found=%v)", h, ok)
			}
		})
	}
}

// 用户在下单途中删掉任务:结账前的任何一步被取消,都必须**一分钱都不花**,
// 而且仍然要把 cart 清干净(清理那步故意不挂 ctx,否则一取消就留下僵尸 cart)。
func TestPurchaseServerCancelBeforeCheckoutNeverOrders(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, availPlainInStock)
	st := newPurchaseTestState(t, f.srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 车建好、商品加好之后用户删任务 —— 最接近真实的时机
	f.route("POST /order/cart/"+testCartID+"/eco", func(w http.ResponseWriter, _ apiCall) {
		cancel()
		fmt.Fprintf(w, `{"itemId":%d}`, testItemID)
	})

	item := testItem()
	out := PurchaseServer(ctx, st, item)

	if !out.Cancelled {
		t.Errorf("应当报 Cancelled,实际 %+v;序列 %v", out, f.seq())
	}
	if out.Success || out.Attempted || out.Fatal {
		t.Errorf("用户自己删的任务不是失败,不该计数也不该判死: %+v", out)
	}
	if n := f.count("POST /order/cart/" + testCartID + "/checkout"); n != 0 {
		t.Errorf("取消之后绝不能结账,实际结账了 %d 次", n)
	}
	if _, ok := historyFor(t, st, item.ID); ok {
		t.Error("用户自己删的任务不该写进购买历史")
	}
	// 清理故意不挂 ctx:挂上的话用户一删任务,清理请求自己先被取消,
	// 僵尸 cart 就留在 OVH 那边了。
	if n := f.count("DELETE /order/cart/" + testCartID); n != 1 {
		t.Errorf("取消路径也必须清掉 cart,实际 DELETE 了 %d 次", n)
	}
}

// 结账请求一旦发出去就**不再接受取消**。
//
// 这是整条链路上最要命的一条:请求已经到了 OVH、我们这边却中途撒手,
// 订单可能已经生成而我们不知道 —— 既没 history 也没通知,
// 用户是在月底账单上发现这台机器的。比"多下了一单"糟得多。
// 实现靠的是 context.WithoutCancel;这个用例就是拿来盯住它别被"顺手统一一下"改掉。
func TestPurchaseServerCheckoutIgnoresCancelOnceSent(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, availPlainInStock)
	st := newPurchaseTestState(t, f.srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 请求已经到 OVH 了,这时候用户才删任务
	f.route("POST /order/cart/"+testCartID+"/checkout", func(w http.ResponseWriter, _ apiCall) {
		cancel()
		time.Sleep(50 * time.Millisecond) // 给取消一点时间生效
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"orderId":%s,"url":"https://example.invalid/order/%s"}`, testOrderID, testOrderID)
	})

	item := testItem()
	out := PurchaseServer(ctx, st, item)

	if !out.Success {
		t.Fatalf("结账已经发出,订单也回来了,必须按成功处理(否则这一单没人知道);实际 %+v", out)
	}
	h, ok := historyFor(t, st, item.ID)
	if !ok || h.Status != "success" || h.OrderID != testOrderID {
		t.Errorf("必须留下成功记录和订单号,否则用户要到账单上才发现这台机器: %+v (found=%v)", h, ok)
	}
	f.waitForCall(t, "GET /me/order/"+testOrderID)
}

// 用户点名要的硬件选项在 OVH 的可用选项里找不到 → 宁可不下单。
// 继续 checkout 会用基础 plan 的默认存储(多半是 HDD)下到错误配置 ——
// 花了钱,拿到的不是要的那台机器,而且退不掉。
func TestPurchaseServerMissingHardwareOptionNeverOrdersWrongConfig(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, `[{"fqn":"24sk602.softraid-2x960nvme","planCode":"24sk602",
	  "datacenters":[{"datacenter":"gra","availability":"24H"}]}]`)
	// 这辆车能选的 eco 选项里没有用户要的那块盘
	f.json("GET /order/cart/"+testCartID+"/eco/options", `[]`)
	st := newPurchaseTestState(t, f.srv.URL)

	item := testItem()
	item.Options = []string{"softraid-2x960nvme"}

	out := PurchaseServer(context.Background(), st, item)

	if n := f.count("POST /order/cart/" + testCartID + "/checkout"); n != 0 {
		t.Errorf("选项对不上就绝不能结账,实际结账了 %d 次;序列 %v", n, f.seq())
	}
	// 用户显式指定的选项对不上该子公司目录 = 确定性失败:eco/options 是按
	// (子公司, planCode) 算出来的固定集合,下一轮重试还是同一份。
	if !out.Fatal {
		t.Errorf("用户显式指定的选项对不上,应当判 Fatal 而不是反复重试刷失败记录: %+v", out)
	}
	if h, ok := historyFor(t, st, item.ID); !ok || h.Status != "failed" {
		t.Errorf("应当留一条 failed 说明原因,实际 %+v (found=%v)", h, ok)
	}
	if n := f.count("DELETE /order/cart/" + testCartID); n != 1 {
		t.Errorf("中止下单后也要清掉 cart,实际 DELETE 了 %d 次", n)
	}
}

// 没货的那一轮:不建车、不下单、不写 history、不计失败次数。
// 抢购的常态就是绝大多数轮次都没货,任何一项做错都会在几分钟内把任务刷死。
func TestPurchaseServerOutOfStockIsAQuietNoOp(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, `[{"fqn":"24sk602","planCode":"24sk602",
	  "datacenters":[{"datacenter":"gra","availability":"unavailable"}]}]`)
	st := newPurchaseTestState(t, f.srv.URL)
	item := testItem()

	out := PurchaseServer(context.Background(), st, item)

	if out.Success || out.Fatal || out.Attempted || out.Cancelled {
		t.Errorf("没货应当是一个安静的空轮(全 false),实际 %+v", out)
	}
	if got := f.seq(); len(got) != 1 {
		t.Errorf("没货时只该查一次库存就返回,实际发了 %v", got)
	}
	if _, ok := historyFor(t, st, item.ID); ok {
		t.Error("没货不是失败,不该写进购买历史 —— 否则抢一晚上能刷出几百条")
	}
}

// comingSoon 是"即将上线尚未开售",不是有货。
// 当成有货的话会为一台永远下不了单的机器跑完整个下单流程,白刷 OVH 的限流额度 ——
// 而限流一旦撞上,真正有货的那一刻反而查不到库存。
func TestPurchaseServerComingSoonIsNotInStock(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, `[{"fqn":"24sk602","planCode":"24sk602",
	  "datacenters":[{"datacenter":"gra","availability":"comingSoon"}]}]`)
	st := newPurchaseTestState(t, f.srv.URL)

	out := PurchaseServer(context.Background(), st, testItem())

	if out.Success || out.Attempted {
		t.Errorf("comingSoon 不算有货,实际 %+v", out)
	}
	if n := f.count("POST /order/cart"); n != 0 {
		t.Errorf("comingSoon 不该建车,实际建了 %d 次", n)
	}
}
