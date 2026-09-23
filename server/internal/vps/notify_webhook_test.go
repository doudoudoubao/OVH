package vps

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ovh-buy/server/internal/app"
	"github.com/ovh-buy/server/internal/config"
	"github.com/ovh-buy/server/internal/db"
	"github.com/ovh-buy/server/internal/logger"
	"github.com/ovh-buy/server/internal/storage"
)

// 只配了 Webhook(没配 Telegram)时,VPS 补货通知也必须发出去。
//
// 以前 SendSummaryNotification 一上来先判"没配 Telegram 就 return false",
// 而后面走的是 notify.Broadcast(Webhook 也能收)。监控循环的自停判据看的是
// "任意通道可用",所以只配 Webhook 的用户:VPS 监控照跑、补货一条通知都没有。
func TestVPSSummaryNotificationReachesWebhookOnlySetup(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	lg := logger.New(filepath.Join(dir, "t.log"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	st := app.NewState(storage.Paths{DataDir: dir}, config.New(database), lg, database)

	var mu sync.Mutex
	var body string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
	}))
	defer hook.Close()
	cfg := st.Config.Get()
	cfg.NotifyWebhookURL = hook.URL
	if err := st.Config.Set(cfg); err != nil {
		t.Fatal(err)
	}

	ok := SendSummaryNotification(st, "vps-2025-model1", "IE",
		[]map[string]interface{}{{"name": "Gravelines", "code": "GRA", "status": "available"}}, "available")
	if !ok {
		t.Fatal("只配了 Webhook 时 VPS 补货通知没有发出")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(body, "VPS补货通知") {
		t.Errorf("Webhook 收到的内容不对: %s", body)
	}
}
