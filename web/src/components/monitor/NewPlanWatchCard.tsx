import { useEffect, useState } from "react";
import { Sparkles, ChevronDown, ChevronUp } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import { Skeleton } from "@/components/common/Skeleton";
import { LoadFailed } from "@/components/common/LoadFailed";
import { useAccounts } from "@/hooks/use-accounts";
import {
  useNewPlanWatch,
  useSaveNewPlanWatch,
  type NewPlanWatchConfig,
} from "@/hooks/use-monitor";

/**
 * 新机型发现器。
 *
 * 目录里冒出没见过的 planCode 时通知你；开了自动下单的话，还会为它自动建一条
 * 「有货就买」的监控订阅（带月费上限）。
 *
 * **这条路径永远不会自动付款。** 订单停在未付款状态，逾期自动作废，
 * 付不付由你自己决定 —— 所以这里没有「自动付款」这个开关，不是默认关，是压根没有。
 */
export function NewPlanWatchCard() {
  const q = useNewPlanWatch();
  const save = useSaveNewPlanWatch();
  const accounts = useAccounts();
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<NewPlanWatchConfig | null>(null);

  // 服务端的值回来了就填进表单。只在没有草稿时填 —— 否则用户正在编辑时
  // 一次后台 refetch 会把他刚敲进去的数字冲掉。
  useEffect(() => {
    if (q.data?.config && !draft) setDraft(q.data.config);
  }, [q.data, draft]);

  if (q.isPending) return <Skeleton className="h-16 rounded-2xl" />;
  if (q.isError) {
    return (
      <Card>
        <LoadFailed
          icon={Sparkles}
          title="新机型发现器配置读取失败"
          error={q.error}
          onRetry={() => void q.refetch()}
        />
      </Card>
    );
  }

  const cfg = draft ?? q.data!.config;
  const hint = q.data!.currencyHint ?? {};
  const hintText = Object.entries(hint)
    .map(([sub, cur]) => `${sub}=${cur}`)
    .join(" · ");
  const set = (patch: Partial<NewPlanWatchConfig>) =>
    setDraft({ ...cfg, ...patch });

  // 开了自动下单却没填全，保存必定被后端拒 —— 提前在按钮上说清楚
  const blocker = cfg.autoOrder
    ? cfg.maxMonthly <= 0
      ? "自动下单必须设月费上限"
      : !cfg.currency.trim()
        ? "月费上限必须写明币种"
        : ""
    : "";

  return (
    <Card>
      <CardContent className="p-4 space-y-3">
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-center gap-2 min-w-0">
            <Sparkles className="size-4 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium">新机型发现</div>
              <div className="text-xs text-muted-foreground truncate">
                {cfg.enabled
                  ? `每 ${cfg.intervalMinutes} 分钟查一次目录` +
                    (cfg.autoOrder
                      ? ` · 自动下单（≤ ${cfg.maxMonthly} ${cfg.currency}/月）`
                      : " · 只通知")
                  : "已关闭"}
                {q.data!.knownPlanCount > 0
                  ? ` · 基线 ${q.data!.knownPlanCount} 个机型`
                  : " · 尚未建立基线"}
              </div>
            </div>
          </div>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
          >
            {open ? <ChevronUp className="size-4" /> : <ChevronDown className="size-4" />}
          </Button>
        </div>

        {open && (
          <div className="space-y-4 pt-1 border-t">
            <label className="flex items-start gap-2 pt-3 cursor-pointer">
              <Checkbox
                checked={cfg.enabled}
                onCheckedChange={(v) => set({ enabled: v === true })}
              />
              <span className="text-sm leading-tight">
                开启新机型发现
                <span className="block text-xs text-muted-foreground">
                  关着的时候连目录都不拉。第一轮只建立基线、不告警 ——
                  否则开机会把目录里几百个机型全当成「新上架」。
                </span>
              </span>
            </label>

            <div className="grid gap-1.5">
              <label className="text-xs text-muted-foreground">
                检查间隔（{q.data!.minIntervalMins}-{q.data!.maxIntervalMins} 分钟）
              </label>
              <Input
                type="number"
                className="h-8 w-32"
                value={cfg.intervalMinutes}
                min={q.data!.minIntervalMins}
                max={q.data!.maxIntervalMins}
                onChange={(e) =>
                  set({ intervalMinutes: Number(e.target.value) || 0 })
                }
              />
              <p className="text-[11px] text-muted-foreground">
                每轮都会真的重拉一份约 12MB 的公开目录（不占账户 API 配额）。
                新机型上架是产品发布级别的事，不是补货那种以秒计的窗口，不必查太勤。
              </p>
            </div>

            <label className="flex items-start gap-2 cursor-pointer">
              <Checkbox
                checked={cfg.autoOrder}
                onCheckedChange={(v) => set({ autoOrder: v === true })}
              />
              <span className="text-sm leading-tight">
                发现新机型后自动下单
                <span className="block text-xs text-muted-foreground">
                  会为它建一条「有货就买」的监控订阅。
                  <strong className="text-foreground">不会自动付款</strong> ——
                  订单停在未付款，逾期自动作废，付不付你自己决定。
                </span>
              </span>
            </label>

            {cfg.autoOrder && (
              <div className="space-y-3 pl-6 border-l">
                <div className="grid gap-1.5">
                  <label className="text-xs text-muted-foreground">月费上限（含税）</label>
                  <div className="flex gap-2">
                    <Input
                      type="number"
                      className="h-8 w-28"
                      step="0.01"
                      value={cfg.maxMonthly || ""}
                      placeholder="15"
                      onChange={(e) =>
                        set({ maxMonthly: Number(e.target.value) || 0 })
                      }
                    />
                    <Input
                      className="h-8 w-24"
                      value={cfg.currency}
                      placeholder="EUR"
                      onChange={(e) =>
                        set({ currency: e.target.value.toUpperCase() })
                      }
                    />
                  </div>
                  <p className="text-[11px] text-muted-foreground">
                    币种必须和账户所在站点一致，本程序
                    <strong className="text-foreground">不做汇率换算</strong>
                    ，对不上就一律不下单。
                    {hintText ? `当前账户站点计价：${hintText}。` : ""}
                    这里填的是基础机型价的预筛；下单前还会按真实配置（含内存/硬盘加价）再卡一次。
                  </p>
                </div>

                <div className="grid gap-1.5">
                  <label className="text-xs text-muted-foreground">下单账户</label>
                  <select
                    className="h-8 rounded-md border bg-background px-2 text-sm w-full max-w-xs"
                    value={cfg.accountId}
                    onChange={(e) => set({ accountId: e.target.value })}
                  >
                    <option value="">按机型所属站点自动挑</option>
                    {(accounts.data ?? []).map((a) => (
                      <option key={a.id} value={a.id}>
                        {a.name}（{a.zone || a.endpoint}）
                      </option>
                    ))}
                  </select>
                  <p className="text-[11px] text-muted-foreground">
                    指定的账户不在新机型所属站点时，不会替你换一张卡 —— 那一条会跳过并说明原因。
                  </p>
                </div>

                <div className="grid gap-1.5">
                  <label className="text-xs text-muted-foreground">单轮最多建几条</label>
                  <Input
                    type="number"
                    className="h-8 w-24"
                    min={1}
                    max={10}
                    value={cfg.maxPerRound}
                    onChange={(e) =>
                      set({ maxPerRound: Number(e.target.value) || 1 })
                    }
                  />
                  <p className="text-[11px] text-muted-foreground">
                    OVH 偶尔一次上架一批。没有这个上限，一轮就能建出十几条自动下单订阅。
                    后端硬顶 10 条（和补货那边「一次触发最多买 10 台」同一个口径）。
                  </p>
                </div>
              </div>
            )}

            <div className="flex items-center gap-2">
              <Button
                size="sm"
                disabled={save.isPending || !!blocker}
                onClick={() => save.mutate(cfg)}
              >
                {save.isPending ? "保存中…" : "保存"}
              </Button>
              {draft && (
                <Button variant="ghost" size="sm" onClick={() => setDraft(null)}>
                  撤销改动
                </Button>
              )}
              {blocker && (
                <span className="text-xs text-destructive">{blocker}</span>
              )}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
