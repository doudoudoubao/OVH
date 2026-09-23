package purchase

// 下单成功的通知:要发得出去,而且要说真话。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 只配了 Webhook(没配 Telegram)也必须收到"下单成功,请尽快付款"。
//
// 以前 PurchaseServer 先判"配了 Telegram 才发",而 notify.Broadcast 本身会发 Webhook ——
// 只用 Webhook 的用户永远收不到这条。未付款订单逾期作废,漏掉它等于可能丢机器。
func TestOrderSuccessNoticeReachesWebhookOnlySetup(t *testing.T) {
	isolatePublicCatalog(t)
	f := newFakeOVH(t)
	scriptOrderFlow(f, availPlainInStock)
	st := newPurchaseTestState(t, f.srv.URL)

	var mu sync.Mutex
	got := []string{}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		msg, _ := body["text"].(string)
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
	}))
	defer hook.Close()
	cfg := st.Config.Get()
	cfg.TgToken, cfg.TgChatID = "", ""
	cfg.NotifyWebhookURL = hook.URL
	if err := st.Config.Set(cfg); err != nil {
		t.Fatal(err)
	}

	if out := PurchaseServer(context.Background(), st, testItem()); !out.Success {
		t.Fatalf("前提:应当下单成功,实际 %+v", out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || !strings.Contains(got[0], "下单成功") {
		t.Fatalf("只配了 Webhook 时没收到下单成功通知,收到: %q", got)
	}
}

// checkout 不再发 waiveRetractationPeriod,撤销权还在。
// 通知里要是还写着"已放弃 14 天撤销期",用户会以为这单退不了。
func TestOrderSuccessMessageDoesNotClaimRetractionWaived(t *testing.T) {
	item := testItem()
	for _, autoPay := range []bool{false, true} {
		item.AutoPay = autoPay
		msg := BuildOrderSuccessMessage(item, "1", "https://example.invalid")
		if strings.Contains(msg, "放弃 14 天撤销期") || strings.Contains(msg, "已按惯例放弃") {
			t.Errorf("autoPay=%v:通知说撤销期已放弃,而 checkout 实际没放弃:\n%s", autoPay, msg)
		}
		if !strings.Contains(msg, "未放弃 14 天无理由撤销权") {
			t.Errorf("autoPay=%v:通知应当说明撤销权还在:\n%s", autoPay, msg)
		}
	}
}
