package catalog

import (
	"sort"
	"strings"
	"time"

	"github.com/ovh-buy/server/internal/app"
)

// DefaultMaxAge 复用那份跨账户共享的目录缓存(2 小时)。
// 绝大多数调用方要的都是它 —— 机型的 region / 机房 / addon 配置不会在两小时里改掉。
const DefaultMaxAge = regionCacheTTL

// AlwaysFresh 缓存一律不算数,每次都真的重拉一份目录。
//
// 只有新机型发现器该用它:那条轮询的全部职责就是"目录里有没有冒出没见过的 planCode",
// 而读缓存意味着最多晚 2 小时才发现,轮询本身就失去了意义。
// 代价是每轮一份 ~12MB 的公开请求(不带凭据,不占账户的 API 配额)。
const AlwaysFresh = time.Duration(0)

// CatalogPlan 目录里一个机型的摘要。
type CatalogPlan struct {
	PlanCode string
	// Monthly 含税月费,**不含 addon**。
	//
	// 这一点对"按价格筛选新机型"很要紧:一台基础价 9 欧的机器,配上 NVMe 和大内存
	// 可以轻松翻倍。所以它只够当一道**预筛**(明显超标的直接不理),
	// 真正管钱的那道闸在 PurchaseServer 结账之前 —— 那时候才知道最终买的是哪套配置。
	Monthly float64
	// MonthlyOK 目录里有没有给出可用的月费计价。false 时 Monthly 无意义。
	MonthlyOK bool
	Currency  string
	// Datacenters 目录里这个机型可选的机房。
	// **不是"有货的机房"** —— 有没有货要问 /dedicated/server/datacenter/availabilities。
	Datacenters []string
}

// ListPlans 列出该子公司目录里的全部机型,按 planCode 排序(稳定输出,便于比对和日志)。
//
// maxAge 见 DefaultMaxAge / AlwaysFresh。
func ListPlans(state *app.State, subsidiary string, maxAge time.Duration) ([]CatalogPlan, error) {
	cat, err := loadSubsidiaryCatalogWithin(state, subsidiary, maxAge)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogPlan, 0, len(cat.plans))
	for code, pc := range cat.plans {
		dcs := make([]string, len(pc.datacenters))
		copy(dcs, pc.datacenters)
		out = append(out, CatalogPlan{
			PlanCode:    code,
			Monthly:     pc.monthly.Total(),
			MonthlyOK:   pc.monthly.OK,
			Currency:    cat.currency,
			Datacenters: dcs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlanCode < out[j].PlanCode })
	return out, nil
}

// CachedCurrency 只从已有缓存里读这个子公司的计价币种,**绝不触发拉取**。
//
// 给同步 HTTP handler 用:走会触发拉取的路径的话,缓存冷的时候一个页面请求
// 就要等一份 12MB 的目录下载完。这类"锦上添花"的信息拿不到就该直接不给,
// 而不是让整个页面等着。
func CachedCurrency(subsidiary string) (string, bool) {
	subsidiary = strings.ToUpper(strings.TrimSpace(subsidiary))
	regionCacheMu.Lock()
	defer regionCacheMu.Unlock()
	c, ok := regionCache[subsidiary]
	if !ok || c == nil {
		return "", false
	}
	cur := strings.TrimSpace(c.currency)
	return cur, cur != ""
}
