package monitor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ovh-buy/server/internal/notify"
)

// tgRecheckInterval loop 内 TG 健康检查节流间隔。5 分钟 verify 一次,
// 失败立即调 Stop() 自停监控。
const tgRecheckInterval = 5 * time.Minute

// maxOrdersPerTrigger 一次检查最多下多少单(跨全部配置累计,见 orderBudget)。
//
// 这是个纯粹的防脚滑护栏,不是业务限制:订阅里的 quantity 没有上限,
// 而真正的单数还要乘上"本轮同时补货的机房数" —— 这个乘法在界面上看不见。
// 抢购本身是花钱的动作,配 autoPay 时更是直接扣款,宁可截断并在日志里说清楚,
// 也不要让一次误配置变成十几台机器。真想要更多就拆成多条订阅。
const maxOrdersPerTrigger = 10

// orderTask 一条待发的入队请求:某个机房的第 idx+1 台
type orderTask struct {
	dc  string
	idx int // 当前机房下的第 idx+1 个,日志用
}

// buildOrderTasks 把「机房集合 × 每个机房要几台」摊平成任务列表,并截到 limit。
//
// 轮次在外、机房在内:排出来是 dc1#1, dc2#1, dc3#1, dc1#2, dc2#2 …
// 这样按 limit 截尾巴天然就是按机房均分,而不是把末尾几个机房整个砍掉
// (被砍掉的那几个可能恰好是用户最想要的)。
func buildOrderTasks(dcs []string, quantity, limit int) []orderTask {
	if quantity <= 0 || limit <= 0 || len(dcs) == 0 {
		return nil
	}
	tasks := make([]orderTask, 0, limit)
	for i := 0; i < quantity; i++ {
		for _, dc := range dcs {
			if len(tasks) >= limit {
				return tasks
			}
			tasks = append(tasks, orderTask{dc: dc, idx: i})
		}
	}
	return tasks
}

// checkNotifyOrStop 节流后体检通知通道,**全部**不可用才自停。
//
// 以前这里只看 Telegram,TG 一失效就停整个监控 —— 于是 bot 被封、token 过期、
// 或者机器一时连不上 api.telegram.org,用户失去的不是一条通知,而是整个补货监控,
// 且只有翻日志才知道。现在只要还有一条通道能送达(比如 Webhook),监控就继续跑。
//
// 返回 true=继续 loop,false=已自停。
func (m *Monitor) checkNotifyOrStop() bool {
	m.tgCheckMu.Lock()
	due := time.Since(m.lastTGCheck) >= tgRecheckInterval
	m.tgCheckMu.Unlock()
	if !due {
		return true
	}
	ok, reason := notify.AnyAvailable(m.state, true)
	m.tgCheckMu.Lock()
	m.lastTGCheck = time.Now()
	m.tgCheckMu.Unlock()
	if !ok {
		m.state.Logger.Error("所有通知通道都不可用,自动停止服务器监控("+reason+
			")。配一条 Webhook 作为备用通道可以避免这种全盲。", "monitor")
		m.Stop()
		return false
	}
	return true
}

func (m *Monitor) runSubscriptionCheck(sub *Subscription, traceID string) {
	planCode := sub.PlanCode
	m.state.Logger.Info("开始处理订阅: "+planCode, "monitor")
	m.CheckAvailabilityChange(sub, traceID)
	m.state.Logger.Info("完成处理订阅: "+planCode, "monitor")
}

// monitorLoop 兼容入口:按当前代际号跑
func (m *Monitor) monitorLoop() {
	m.subsMu.Lock()
	gen := m.generation
	m.subsMu.Unlock()
	m.monitorLoopGen(gen)
}

// stillMine 这个循环该不该继续:running 为真且代际号还是自己那一代
func (m *Monitor) stillMine(gen int64) bool {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	return m.running && m.generation == gen
}

func (m *Monitor) monitorLoopGen(gen int64) {
	m.state.Logger.Info("监控循环已启动", "monitor")
	for {
		if !m.stillMine(gen) {
			break
		}

		// 通知通道全挂 → 自停。checkNotifyOrStop 内部已节流 5 分钟,且失败时调 Stop()。
		if !m.checkNotifyOrStop() {
			break
		}

		m.cleanupExpiredCaches()

		m.subsMu.Lock()
		count := len(m.subscriptions)
		subsCopy := make([]*Subscription, count)
		copy(subsCopy, m.subscriptions)
		interval := m.checkInterval
		m.subsMu.Unlock()

		if count > 0 {
			m.state.Logger.Info(fmt.Sprintf("开始检查 %d 个订阅...", count), "monitor")
			workers := m.maxWorkers
			if count < workers {
				workers = count
			}
			if workers < 1 {
				workers = 1
			}
			sem := make(chan struct{}, workers)
			var wg sync.WaitGroup
			for _, sub := range subsCopy {
				running := m.stillMine(gen)
				if !running {
					break
				}
				if !m.stillInSubscriptions(sub) {
					m.state.Logger.Debug(fmt.Sprintf("订阅 %s 在检查期间被删除，跳过", sub.PlanCode), "monitor")
					continue
				}
				traceID := uuid.NewString()
				wg.Add(1)
				sem <- struct{}{}
				go func(s *Subscription, tid string) {
					defer wg.Done()
					defer func() { <-sem }()
					defer func() {
						if r := recover(); r != nil {
							m.state.Logger.Error(fmt.Sprintf("[trace:%s] 并发检查订阅 %s 时异常: %v",
								tid, s.PlanCode, r), "monitor")
						}
					}()
					m.runSubscriptionCheck(s, tid)
				}(sub, traceID)
			}
			wg.Wait()

			// 本轮改出了新状态就落库。
			// 以前这一步根本不存在:LastStatus / History 只活在内存里,
			// 只有用户手动增删改订阅时才顺带被写进去。程序重启后 LastStatus 是空的,
			// 下一轮就把"现在有货"误判成首次检查的初始存量 —— 不通知,也不自动下单。
			// 用户重启一次,就正好错过一次补货。
			if m.dirty.Swap(false) {
				m.SaveToDB()
			}
		} else {
			m.state.Logger.Info("当前无订阅，跳过检查", "monitor")
		}

		// 等下次（可中断 sleep）
		running := m.stillMine(gen)
		if running {
			m.state.Logger.Info(fmt.Sprintf("等待 %d 秒后进行下次检查...", interval), "monitor")
			for i := 0; i < interval; i++ {
				running = m.stillMine(gen)
				if !running {
					break
				}
				time.Sleep(time.Second)
			}
		}
	}
	m.state.Logger.Info("监控循环已停止", "monitor")
}

func (m *Monitor) stillInSubscriptions(sub *Subscription) bool {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for _, s := range m.subscriptions {
		if s == sub {
			return true
		}
	}
	return false
}

func (m *Monitor) Start() bool {
	m.subsMu.Lock()
	if m.running {
		m.subsMu.Unlock()
		m.state.Logger.Warn("监控已在运行中", "monitor")
		return false
	}
	m.running = true
	m.generation++
	gen := m.generation
	// MonitorRunning 与 checkInterval 都要在锁内取/写:
	// Start 可能与 loop 自停时调的 Stop、以及 SetCheckInterval 并发,
	// 出锁之后再写就是无同步的写-写竞争(go test -race 会报)。
	m.state.MonitorRunning = true
	interval := m.checkInterval
	m.subsMu.Unlock()
	// 重置 TG 检查时间戳,保证启动后第一轮一定 verify
	m.tgCheckMu.Lock()
	m.lastTGCheck = time.Time{}
	m.tgCheckMu.Unlock()
	go m.monitorLoopGen(gen)
	m.state.Logger.Info(fmt.Sprintf("服务器监控已启动 (检查间隔: %d秒)", interval), "monitor")
	return true
}

func (m *Monitor) Stop() bool {
	m.subsMu.Lock()
	if !m.running {
		m.subsMu.Unlock()
		m.state.Logger.Warn("监控未运行", "monitor")
		return false
	}
	m.running = false
	m.state.MonitorRunning = false
	m.subsMu.Unlock()
	m.state.Logger.Info("正在停止服务器监控...", "monitor")
	return true
}

// batchOrder 监控触发的批量下单:逐个调本地 quick-order 入队。
// accountID:auto_order 账户;空时 batchOrder 不应该被调到(check.go 的 guard 已挡住),
// 这里再做一次防御性检查。
//
// limit 是本轮检查**还剩**的下单额度(见 orderBudget)。batchOrder 是按配置逐套调的,
// 封顶必须由调用方跨配置累计,这里只负责不超过它。返回成功入队的单数。
func (m *Monitor) batchOrder(planCode string, configInfo map[string]interface{}, targets []notification, quantity int, accountID string, autoPay bool, maxMonthly float64, maxMonthlyCurrency string, limit int) int {
	if accountID == "" {
		m.state.Logger.Warn("[monitor->order] 跳过自动下单: 订阅未指定 auto_order 账户", "monitor")
		return 0
	}
	if limit <= 0 {
		m.state.Logger.Warn(fmt.Sprintf("[monitor->order] %s 本轮下单额度已用完,这套配置不再下单", planCode), "monitor")
		return 0
	}
	if quantity < 0 {
		quantity = 0
	}
	totalOrders := len(targets) * quantity
	// 总量封顶。订阅的 quantity 只校验了下限(<1 → 1),没有上限,而这里还要再乘
	// "本轮同时补货的机房数" —— 一个机型在 5 个机房同时上架、quantity 填 3,
	// 一次跳变就是 15 单。用户填 quantity 时想的是"每个机房买几台",
	// 不是"乘上机房数之后一共几台",这个乘法没人在界面上看得见。
	// 超了就按机房轮流截断(而不是砍掉末尾几个机房),让每个机房都还有机会。
	if totalOrders > limit {
		m.state.Logger.Warn(fmt.Sprintf(
			"[monitor->order] %s 本轮 %d 个机房 × 数量 %d = %d 单,超过本轮剩余下单额度 %d,已截断 —— "+
				"如果确实想要这么多,把订阅拆成多条或调低 quantity",
			planCode, len(targets), quantity, totalOrders, limit), "monitor")
		totalOrders = limit
	}
	m.state.Logger.Info(fmt.Sprintf("[monitor->order] 开始批量下单: %s, 配置数=1, 数据中心数=%d, 数量=%d, 总订单数=%d",
		planCode, len(targets), quantity, totalOrders), "monitor")
	m.state.Logger.Info("[monitor->order] 下单条件：仅对从无货变有货的情况下单（过滤掉持续有货的情况）", "monitor")

	options := []string{}
	if configInfo != nil {
		if opts, ok := configInfo["options"].([]string); ok {
			options = opts
		} else if optsRaw, ok := configInfo["options"].([]interface{}); ok {
			for _, o := range optsRaw {
				if s, ok := o.(string); ok {
					options = append(options, s)
				}
			}
		}
	}

	// 把 N 个 DC × M 个数量打成一个任务列表后并发发出去。
	// 调用的是本地 /api/queue/quick-order(只是入队,不真去 OVH),所以并发完全安全;
	// 也不会冲击 OVH —— 真的下单在 ProcessQueueLoop 里按 concurrentBatchSize=10 节流跑。
	dcs := make([]string, 0, len(targets))
	for _, n := range targets {
		dcs = append(dcs, n.dc)
	}
	tasks := buildOrderTasks(dcs, quantity, totalOrders)

	var successCount, failCount int64
	httpClient := &http.Client{Timeout: 30 * time.Second}
	postOne := func(t orderTask) {
		payload := map[string]interface{}{
			"account_id":         accountID,
			"planCode":           planCode,
			"datacenter":         t.dc,
			"options":            options,
			"fromMonitor":        true,
			"skipDuplicateCheck": true,
			// 订阅上显式开了才带过去;默认 false,不替用户扣钱
			"autoPay": autoPay,
			// 月费上限。0 = 不限(用户手建的订阅都是 0,行为不变)。
			// 带过去只是为了让它跟着任务进队列 —— 真正的判定在
			// PurchaseServer 结账之前,那时 addon 才刚从有货的 FQN 推出来。
			"maxMonthly":         maxMonthly,
			"maxMonthlyCurrency": maxMonthlyCurrency,
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost,
			"http://127.0.0.1:"+m.state.Port+"/api/queue/quick-order",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", m.state.APIKey)

		m.state.Logger.Info(fmt.Sprintf("[monitor->order] 尝试快速下单 (%d/%d): %s@%s, options=%v",
			t.idx+1, quantity, planCode, t.dc, options), "monitor")

		resp, err := httpClient.Do(req)
		if err != nil {
			atomic.AddInt64(&failCount, 1)
			m.state.Logger.Warn(fmt.Sprintf("[monitor->order] 快速下单请求异常 (%d/%d): %s",
				t.idx+1, quantity, err.Error()), "monitor")
			return
		}
		respBody := make([]byte, 0, 1024)
		buf := make([]byte, 1024)
		for {
			nr, rerr := resp.Body.Read(buf)
			if nr > 0 {
				respBody = append(respBody, buf[:nr]...)
			}
			if rerr != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			atomic.AddInt64(&successCount, 1)
			m.state.Logger.Info(fmt.Sprintf("[monitor->order] 快速下单成功 (%d/%d): %s@%s",
				t.idx+1, quantity, planCode, t.dc), "monitor")
		} else {
			atomic.AddInt64(&failCount, 1)
			m.state.Logger.Warn(fmt.Sprintf("[monitor->order] 快速下单失败 (%d/%d, %d): %s",
				t.idx+1, quantity, resp.StatusCode, string(respBody)), "monitor")
		}
	}

	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(t orderTask) {
			defer wg.Done()
			postOne(t)
		}(t)
	}
	wg.Wait()

	m.state.Logger.Info(fmt.Sprintf("[monitor->order] 批量下单完成: 成功=%d, 失败=%d, 总计=%d",
		atomic.LoadInt64(&successCount), atomic.LoadInt64(&failCount), totalOrders), "monitor")
	return int(atomic.LoadInt64(&successCount))
}

// orderBudget 一次检查(一条订阅的一整轮,跨它的全部配置)最多下几单。
//
// 以前封顶是在 batchOrder 里按**单次调用**截的,而 batchOrder 是按配置逐套调的 ——
// 于是"一次补货最多 10 单"实际是"每套配置最多 10 单":盯全部配置的订阅,
// 4 套配置 × 4 个机房同时上架就是 16 单,每单都单独通过月费闸。
//
// 程序自己建的订阅(新机型自动下单)再收紧到 quantity 台:用户开这个功能的意思是
// "新机型出来给我抢一台",不是"每套内存/硬盘组合 × 每个机房各一台"。
// 未付款订单也会锁机器,多出来的每一张都要用户自己去取消。
func orderBudget(cfg subCheckConfig) int {
	budget := maxOrdersPerTrigger
	if cfg.AutoCreatedFrom != "" {
		q := cfg.Quantity
		if q < 1 {
			q = 1
		}
		if q < budget {
			budget = q
		}
	}
	return budget
}
