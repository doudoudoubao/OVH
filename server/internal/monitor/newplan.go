package monitor

// 新机型发现器。
//
// # 它补的是什么洞
//
// 这个仓库里一直有 CheckNewServers / SendNewServerAlert / knownServers 一整套
// "新机型上架告警"的代码,连持久化和读写 API 都写好了 —— 但 git log -S 查下来,
// 从 2026-05-17 迁到 Go 的那天起,CheckNewServers **一个调用方都没有**。
// 也就是说库里那张 monitor_known_servers 一直是空的,这个功能从来没跑过一次。
// 机型列表也只在用户打开页面或点刷新时才拉(而且有 2 小时缓存)。
//
// 这里把它接上:后台按自己的节奏拉目录 → 比对 → 新机型推通知 →
// (可选)自动建一条带月费上限的监控订阅,剩下的交给已有的监控循环。
//
// # 为什么是"建订阅"而不是直接下单
//
// 新机型刚上架时多半还没有货。直接下单等于下不出去,而"等它有货再买"正是
// 监控循环已经做得很好的事 —— 库存跳变检测、按机房逐套触发、下单去重、
// 单次触发封顶,全都是现成且测过的。所以这里只负责"发现"和"建档",
// 一个字节的下单逻辑都不重写。
//
// # 钱的问题
//
// 两道闸:
//  1. 这里用目录基础月费做**预筛** —— 明显超标的直接不建自动下单的订阅。
//     基础价不含 addon(内存/硬盘),所以它只是预筛,不是判据。
//  2. 真正算数的那道在 PurchaseServer 结账之前(purchase/pricecap.go),
//     那时候 addon 才刚从"当时有货的那条 FQN"推出来,价格才是真的。
//
// 而且**自动付款这条线根本不接**:建出来的订阅 AutoPay 恒为零值 false,
// 下面也没有任何地方去写它。不是"默认关",是压根没有这个开关。
// 订单会停在 notPaid,逾期自动作废,要不要付由用户自己决定。

import (
	"fmt"
	"strings"
	"time"

	"github.com/ovh-buy/server/internal/catalog"
	"github.com/ovh-buy/server/internal/notify"
	"github.com/ovh-buy/server/internal/types"
)

// listPlans 指向 catalog.ListPlans。
//
// 做成包级变量是为了能在测试里换掉它 —— 这一轮的逻辑(按子公司分别建基线、
// 单轮上限、建完订阅顺带把监控拉起来)全都只能在"跑完一整轮"的粒度上验,
// 而真去拉一份 12MB 的公开目录既慢又不可控。
// catalog 包自己也是这么做的(region.go 的 fetchSubsidiaryCatalog)。
var listPlans = catalog.ListPlans

// kvNewPlanWatch 配置在 kv 表里的键
const kvNewPlanWatch = "new_plan_watch"

const (
	// NewPlanMinInterval 轮询间隔下限。
	//
	// 每轮都真的重拉一份 ~12MB 的公开目录(见 catalog.AlwaysFresh 的说明),
	// 拉太勤就是拿自己的带宽换几分钟的发现速度。而"新机型上架"是产品发布级别的
	// 事件,不是补货那种以秒计的窗口 —— 15 分钟已经足够激进。
	NewPlanMinInterval = 15
	// NewPlanMaxInterval 上限,再长就失去意义了
	NewPlanMaxInterval = 24 * 60
	// NewPlanDefaultInterval 默认一小时
	NewPlanDefaultInterval = 60
	// NewPlanDefaultMaxPerRound 单轮最多为几个新机型建自动下单订阅。
	//
	// OVH 偶尔一次上架一批。没有这个上限的话,一轮就能建出十几条自动下单订阅,
	// 然后在它们陆续有货时给你下十几张未付款订单 —— 每一张都要你自己去看、去取消。
	NewPlanDefaultMaxPerRound = 2
	// NewPlanMaxPerRound 单轮上限的硬顶。
	//
	// 光挡下限是不够的:界面上填 999 一样会被存下来,那一轮就能建出 999 条
	// 自动下单订阅。和补货那边的 maxOrdersPerTrigger 取同一个数 ——
	// "一次触发最多给你买 10 台"是这个程序一贯的口径。
	NewPlanMaxPerRound = 10
)

// NewPlanWatchConfig 新机型发现器的配置。默认值(零值)= 全关。
type NewPlanWatchConfig struct {
	// Enabled 是否开启轮询。关掉时连目录都不拉。
	Enabled bool `json:"enabled"`
	// IntervalMinutes 轮询间隔(分钟)
	IntervalMinutes int `json:"intervalMinutes"`
	// AutoOrder 发现新机型后是否自动建"有货就买"的订阅。
	// false = 只推通知,什么都不建。和 Enabled 分开:
	// "我想知道有新机型"和"我想让它自动买"是两件事。
	AutoOrder bool `json:"autoOrder"`
	// MaxMonthly 月费上限(含税)。自动下单时必填且必须 > 0 ——
	// 没有上限的自动下单是一张空白支票,这里不接受。
	MaxMonthly float64 `json:"maxMonthly"`
	// Currency 上限的币种(EUR / USD / CAD ...)。必须和账户所在站点的计价币种一致,
	// 对不上时下单闸会拒绝 —— 本程序不做汇率换算。
	Currency string `json:"currency"`
	// AccountID 用哪个账户下单。空 = 按机型所属子公司自动挑一个。
	AccountID string `json:"accountId"`
	// MaxPerRound 单轮最多建几条自动下单订阅
	MaxPerRound int `json:"maxPerRound"`
}

// ClampNewPlanInterval 把间隔夹回合法区间
func ClampNewPlanInterval(v int) int {
	if v < NewPlanMinInterval {
		return NewPlanMinInterval
	}
	if v > NewPlanMaxInterval {
		return NewPlanMaxInterval
	}
	return v
}

// normalized 填好默认值的副本
func (c NewPlanWatchConfig) normalized() NewPlanWatchConfig {
	if c.IntervalMinutes == 0 {
		c.IntervalMinutes = NewPlanDefaultInterval
	}
	c.IntervalMinutes = ClampNewPlanInterval(c.IntervalMinutes)
	if c.MaxPerRound < 1 {
		c.MaxPerRound = NewPlanDefaultMaxPerRound
	}
	if c.MaxPerRound > NewPlanMaxPerRound {
		c.MaxPerRound = NewPlanMaxPerRound
	}
	c.Currency = strings.ToUpper(strings.TrimSpace(c.Currency))
	c.AccountID = strings.TrimSpace(c.AccountID)
	return c
}

// canAutoOrder 这份配置到底能不能自动下单,以及不能的原因。
//
// 每个条件都必须显式满足 —— 自动下单是程序替用户掏钱(虽然停在未付款),
// 任何一项含糊都按"不下单"处理。
func (c NewPlanWatchConfig) canAutoOrder() (bool, string) {
	if !c.AutoOrder {
		return false, "未开启自动下单"
	}
	if c.MaxMonthly <= 0 {
		return false, "没有设月费上限（没有上限的自动下单是一张空白支票，不接受）"
	}
	if c.Currency == "" {
		return false, "月费上限没写币种（15 不说是哪国的 15 就不是一个上限）"
	}
	return true, ""
}

// NewPlanConfig 读当前配置(带默认值)。
func (m *Monitor) NewPlanConfig() NewPlanWatchConfig {
	var cfg NewPlanWatchConfig
	if m.state != nil && m.state.DB != nil {
		if ok, err := m.state.DB.GetKV(kvNewPlanWatch, &cfg); err != nil {
			m.state.Logger.Warn("读新机型发现器配置失败,按默认(全关)处理: "+err.Error(), "monitor")
		} else if !ok {
			return NewPlanWatchConfig{}.normalized()
		}
	}
	return cfg.normalized()
}

// SetNewPlanConfig 写配置。
func (m *Monitor) SetNewPlanConfig(cfg NewPlanWatchConfig) (NewPlanWatchConfig, error) {
	cfg = cfg.normalized()
	if m.state == nil || m.state.DB == nil {
		return cfg, fmt.Errorf("数据库不可用")
	}
	if err := m.state.DB.SetKV(kvNewPlanWatch, cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// watchedSubsidiaries 账户表里出现过的全部子公司(去重)。
// 目录是按子公司分的,US 账户看不到 EU 的机型,反之亦然。
func (m *Monitor) watchedSubsidiaries() []string {
	m.state.AccountsMu.RLock()
	defer m.state.AccountsMu.RUnlock()
	seen := map[string]struct{}{}
	out := []string{}
	for _, a := range m.state.Accounts {
		sub := catalog.SubsidiaryOfAccount(a)
		if sub == "" {
			continue
		}
		if _, ok := seen[sub]; ok {
			continue
		}
		seen[sub] = struct{}{}
		out = append(out, sub)
	}
	return out
}

// accountForSubsidiary 挑一个属于该子公司的账户。prefer 非空且确实属于该子公司时优先。
//
// 不能随便拿个账户去下单:三个站点的目录、库存、购物车全不互通,
// 拿 EU 账户去买美区机型,下单链路会在建车那一步 400。
func (m *Monitor) accountForSubsidiary(subsidiary, prefer string) (types.OVHAccount, bool) {
	m.state.AccountsMu.RLock()
	defer m.state.AccountsMu.RUnlock()
	var fallback types.OVHAccount
	found := false
	for _, a := range m.state.Accounts {
		if !strings.EqualFold(catalog.SubsidiaryOfAccount(a), subsidiary) {
			continue
		}
		if prefer != "" && a.ID == prefer {
			return a, true
		}
		if !found {
			fallback, found = a, true
		}
	}
	if prefer != "" && found {
		// 用户指定的账户不在这个子公司 —— 不静默换一个:他指定账户往往是因为
		// 只想用那张卡。换了等于用另一张卡扣钱。
		return types.OVHAccount{}, false
	}
	return fallback, found
}

// diffNewPlans 比对出这个子公司目录里新冒出来的机型,并把基线更新到当前快照。
//
// **基线是按子公司各记各的**,不能拿 len(knownServers)==0 当"第一次"的判据:
// 目录按子公司分,一轮里要逐个拉,第一个子公司建完基线后那个集合就非空了,
// 第二个子公司的整份目录会被判成新上架 —— 开了自动下单就是当场锁一批货。
// 同一个坑的另外两条路:后来新增一个别的区的账户、某个子公司本轮拉取失败下轮才成功。
//
// 首次见到某个子公司时只建基线、不报任何"新机型":否则一上来就会把那个区
// 三四百个机型全当成新上架推给用户,并照着买。
//
// 返回的是这个子公司"这一轮新增"的那些机型。
func (m *Monitor) diffNewPlans(subsidiary string, plans []catalog.CatalogPlan) []catalog.CatalogPlan {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()

	if m.knownServers == nil {
		m.knownServers = map[string]struct{}{}
	}
	if m.baselinedSubs == nil {
		m.baselinedSubs = map[string]struct{}{}
	}
	_, baselined := m.baselinedSubs[subsidiary]

	var fresh []catalog.CatalogPlan
	for _, p := range plans {
		if p.PlanCode == "" {
			continue
		}
		if _, seen := m.knownServers[p.PlanCode]; !seen {
			m.knownServers[p.PlanCode] = struct{}{}
			if baselined {
				fresh = append(fresh, p)
			}
		}
	}
	if !baselined {
		m.baselinedSubs[subsidiary] = struct{}{}
		m.state.Logger.Info(fmt.Sprintf("[新机型] %s 首次建立基线: %d 个机型（这一轮不告警）",
			subsidiary, len(plans)), "monitor")
		return nil
	}
	return fresh
}

// BaselinedSubsidiaries 已建过基线的子公司(用于持久化)
func (m *Monitor) BaselinedSubsidiaries() []string {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	out := make([]string, 0, len(m.baselinedSubs))
	for k := range m.baselinedSubs {
		out = append(out, k)
	}
	return out
}

// KnownPlanCount 当前基线里有多少个机型(给状态接口用)
func (m *Monitor) KnownPlanCount() int {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	return len(m.knownServers)
}

// NewPlanLoop 新机型轮询。由 main 起一个 goroutine 跑,进程生命周期内一直在。
//
// 开关是每轮读的,不是启动时读一次 —— 用户在界面上打开/关闭要立刻生效,
// 不用重启。关着的时候连目录都不拉。
func (m *Monitor) NewPlanLoop() {
	m.state.Logger.Info("[新机型] 轮询已启动", "monitor")
	for {
		cfg := m.NewPlanConfig()
		if cfg.Enabled {
			m.runNewPlanRound(cfg)
		}
		// 间隔按当前配置走,但**切成小片睡**,每片重读一次配置。
		//
		// 一觉睡满的话,用户把间隔设成 24 小时之后想改回 15 分钟,
		// 得等满这 24 小时才生效;关掉开关同理。切片之后最多迟 30 秒。
		wait := time.Duration(cfg.IntervalMinutes) * time.Minute
		if !cfg.Enabled {
			wait = time.Minute
		}
		sleepInSlices(wait, func() bool {
			next := m.NewPlanConfig()
			// 开关或间隔被改了就立刻重新排期
			return next.Enabled != cfg.Enabled || next.IntervalMinutes != cfg.IntervalMinutes
		})
	}
}

// runNewPlanRound 跑一轮:逐个子公司拉目录 → 比对 → 处理新机型。
func (m *Monitor) runNewPlanRound(cfg NewPlanWatchConfig) {
	created := false
	// 收尾动作放 defer 里:**必须无论怎么退出都跑到**。
	//
	// 建订阅发生在通知之前,而通知那一步是会 panic 的(渲染、Broadcast)。
	// 收尾要是留在函数体末尾,一次 panic 就留下一个半截状态:
	// 订阅在内存里建好了,却既没落库、也没把监控拉起来 —— 那条订阅不会被检查,
	// 重启之后还会凭空消失。
	defer func() {
		if r := recover(); r != nil {
			m.state.Logger.Error(fmt.Sprintf("[新机型] 本轮异常,已跳过: %v", r), "monitor")
		}
		// 刚建了订阅就得保证监控真的在跑。
		//
		// main.go 只在启动时「**已经有订阅**才自动启动」监控(len(mon.Snapshot()) > 0)。
		// 于是全新安装、一条订阅都没有的用户,打开新机型发现 + 自动下单之后:
		// 这里建出了订阅,但监控循环根本没起来,那条订阅永远不会被检查 ——
		// 什么都不发生,而且没有任何提示。正是最难查的那种失效。
		//
		// Start() 自己要取 subsMu,所以必须在 subscribeNewPlan 放锁**之后**调,
		// 不能塞进那个函数里 —— 那是直接死锁。
		if created && m.Start() {
			m.state.Logger.Info("[新机型] 因为新建了自动下单订阅，已顺带启动监控", "monitor")
		}
		// 基线变了就落库。不落的话每次重启都要重新建基线,
		// 而建基线那一轮是不告警的 —— 等于重启一次就漏掉一个发现窗口。
		m.SaveToDB()
	}()

	subs := m.watchedSubsidiaries()
	if len(subs) == 0 {
		m.state.Logger.Debug("[新机型] 还没有任何 OVH 账户，跳过", "monitor")
		return
	}

	ordered := 0
	for _, sub := range subs {
		// AlwaysFresh:读缓存的话最多晚 2 小时才发现,这条轮询就失去意义了
		plans, err := listPlans(m.state, sub, catalog.AlwaysFresh)
		if err != nil {
			m.state.Logger.Warn(fmt.Sprintf("[新机型] 拉 %s 目录失败,本轮跳过它: %s", sub, err.Error()), "monitor")
			continue
		}
		fresh := m.diffNewPlans(sub, plans)
		if len(fresh) == 0 {
			m.state.Logger.Debug(fmt.Sprintf("[新机型] %s 目录 %d 个机型，无新增", sub, len(plans)), "monitor")
			continue
		}
		m.state.Logger.Info(fmt.Sprintf("[新机型] %s 新增 %d 个机型", sub, len(fresh)), "monitor")
		for _, p := range fresh {
			if m.handleNewPlan(cfg, sub, p, ordered) {
				ordered++
				created = true
			}
		}
	}
}

// handleNewPlan 处理一个新机型:推通知,并在满足全部条件时建一条自动下单订阅。
// 返回是否真的建了自动下单订阅(调用方用它计数,以遵守单轮上限)。
func (m *Monitor) handleNewPlan(cfg NewPlanWatchConfig, subsidiary string, p catalog.CatalogPlan, alreadyOrdered int) (armedOut bool) {
	// 逐个机型兜底:一条通知渲染炸了,不该把这一轮剩下的机型一起带走
	// (它们的 planCode 已经进基线了,带走就等于永远不会再被报一次)。
	defer func() {
		if r := recover(); r != nil {
			m.state.Logger.Error(fmt.Sprintf("[新机型] 处理 %s 时异常: %v", p.PlanCode, r), "monitor")
		}
	}()

	priceText := "价格未知"
	if p.MonthlyOK {
		priceText = fmt.Sprintf("%s/月", formatAmount(p.Currency, p.Monthly))
	}

	decision, armed := m.decideNewPlanOrder(cfg, subsidiary, p, alreadyOrdered)
	// 先把"建没建订阅"定下来再发通知:通知是会 panic 的那一步,
	// 而上面那句可能已经建出了订阅 —— 具名返回值 + defer 保证这个事实不会丢。
	armedOut = armed
	m.sendNewPlanAlert(subsidiary, p, priceText, decision)
	return armedOut
}

// decideNewPlanOrder 决定这个新机型要不要建自动下单订阅,并在要建时把订阅建出来。
// 返回 (给用户看的一句话结论, 是否真的建了)。
//
// 所有"不建"的路径都必须给出**具体原因** —— 用户开了自动下单却什么都没发生时,
// 唯一能自救的信息就是这句话。
func (m *Monitor) decideNewPlanOrder(cfg NewPlanWatchConfig, subsidiary string, p catalog.CatalogPlan, alreadyOrdered int) (string, bool) {
	if ok, why := cfg.canAutoOrder(); !ok {
		return "未自动下单：" + why, false
	}
	if alreadyOrdered >= cfg.MaxPerRound {
		return fmt.Sprintf("未自动下单：本轮已经建了 %d 条自动下单订阅，到达单轮上限 %d（想要就手动加监控）",
			alreadyOrdered, cfg.MaxPerRound), false
	}
	if !p.MonthlyOK {
		// 目录没给月费。当成 0 元会让它"最便宜"从而必定通过上限 —— 必须当成不通过。
		return "未自动下单：目录里没给这个机型的月费，价格未知时一律不自动买", false
	}
	gotCur := strings.ToUpper(strings.TrimSpace(p.Currency))
	if gotCur != cfg.Currency {
		return fmt.Sprintf("未自动下单：上限币种是 %s，而 %s 站点按 %s 计价，本程序不做汇率换算",
			cfg.Currency, subsidiary, gotCur), false
	}
	// 预筛用的是**基础价**(不含内存/硬盘 addon)。这里放过去不代表最终能买 ——
	// 真正算数的上限闸在 PurchaseServer 结账之前,那时才知道最终配置。
	if p.Monthly > cfg.MaxMonthly {
		return fmt.Sprintf("未自动下单：基础月费 %s 已超过上限 %s",
			formatAmount(p.Currency, p.Monthly), formatAmount(cfg.Currency, cfg.MaxMonthly)), false
	}
	acc, ok := m.accountForSubsidiary(subsidiary, cfg.AccountID)
	if !ok {
		return fmt.Sprintf("未自动下单：没有属于 %s 站点的可用账户（指定的下单账户不在这个站点时不会替你换一张卡）",
			subsidiary), false
	}
	if m.subscribeNewPlan(cfg, p, acc) {
		return fmt.Sprintf("已自动建监控并开启自动下单（账户 %s，上限 %s/月，**不会自动付款**）",
			acc.Name, formatAmount(cfg.Currency, cfg.MaxMonthly)), true
	}
	return "未自动下单：这个机型已经在监控列表里了", false
}

// subscribeNewPlan 为新机型建一条"有货就买"的订阅。已存在则不动它(用户可能自己配过)。
//
// 注意这里**没有任何一处写 AutoPay** —— 它保持零值 false。
// 不是"默认关",是这条路径压根没有这个开关:订单会停在 notPaid,
// 逾期自动作废,付不付由用户自己决定。
func (m *Monitor) subscribeNewPlan(cfg NewPlanWatchConfig, p catalog.CatalogPlan, acc types.OVHAccount) bool {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for _, s := range m.subscriptions {
		if s.PlanCode == p.PlanCode {
			return false
		}
	}
	m.subscriptions = append(m.subscriptions, &Subscription{
		PlanCode: p.PlanCode,
		// 空 = 盯全部机房。新机型还不知道哪个机房先有货,限定机房等于自缚手脚。
		Datacenters:        []string{},
		NotifyAvailable:    true,
		NotifyUnavailable:  false,
		LastStatus:         map[string]string{},
		CreatedAt:          types.NowISO(),
		History:            []HistoryEntry{},
		ServerName:         p.PlanCode,
		AutoOrder:          true,
		Quantity:           1,
		AutoOrderAccountID: acc.ID,
		// AutoPay 故意留零值,见上面的说明。
		MaxMonthly:         cfg.MaxMonthly,
		MaxMonthlyCurrency: cfg.Currency,
		AutoCreatedFrom:    "new-plan-watch",
	})
	m.state.Logger.Info(fmt.Sprintf(
		"[新机型] 已为 %s 建自动下单订阅（账户 %s，月费上限 %s，不自动付款）",
		p.PlanCode, acc.Name, formatAmount(cfg.Currency, cfg.MaxMonthly)), "monitor")
	return true
}

// sendNewPlanAlert 新机型通知。
//
// 比旧的 SendNewServerAlert 多的是"然后呢":价格、站点、可选机房,
// 以及这一条到底有没有被自动下单、没有的话为什么。
// 半夜收到一条"发现新机型 24sk99"而不知道接下来会发生什么,是没有用的通知。
func (m *Monitor) sendNewPlanAlert(subsidiary string, p catalog.CatalogPlan, priceText, decision string) {
	var b strings.Builder
	b.WriteString("🆕 发现新机型\n\n")
	b.WriteString("型号: " + p.PlanCode + "\n")
	b.WriteString("月费: " + priceText + "（基础价，不含内存/硬盘加价）\n")
	b.WriteString("站点: " + subsidiary + "\n")
	if len(p.Datacenters) > 0 {
		dcs := p.Datacenters
		if len(dcs) > 8 {
			dcs = append(append([]string{}, dcs[:8]...), fmt.Sprintf("…共%d个", len(p.Datacenters)))
		}
		b.WriteString("可选机房: " + strings.Join(dcs, " ") + "\n")
	}
	b.WriteString("时间: " + m.nowBeijing().Format("2006-01-02 15:04:05") + "\n\n")
	b.WriteString(decision)
	notify.Broadcast(m.state, b.String(), nil)
	m.state.Logger.Info(fmt.Sprintf("[新机型] 已通知 %s（%s）: %s", p.PlanCode, priceText, decision), "monitor")
}

// newPlanSleepSlice 轮询等待切片的粒度。做成变量只为了让测试能调小 ——
// 否则验"配置一变就提前醒"的用例自己要跑满一片。
var newPlanSleepSlice = 30 * time.Second

// sleepInSlices 把一段等待切成小片,每片之间问一次 changed()。
// changed() 返回 true 就提前醒来 —— 好让配置改动最多迟一片的时间生效,
// 而不是等满用户设的那个间隔(最长可以是 24 小时)。
func sleepInSlices(total time.Duration, changed func() bool) {
	slice := newPlanSleepSlice
	for left := total; left > 0; left -= slice {
		d := slice
		if left < d {
			d = left
		}
		time.Sleep(d)
		if changed != nil && changed() {
			return
		}
	}
}
