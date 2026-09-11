package monitor

import "testing"

// buildOrderTasks 要同时满足两件事:
//  1. 不封顶时,每个机房各 quantity 台,一台不少(quantity 的语义);
//  2. 封顶时按机房均分,而不是把末尾几个机房整个砍掉。
func TestBuildOrderTasks(t *testing.T) {
	count := func(tasks []orderTask) map[string]int {
		m := map[string]int{}
		for _, t := range tasks {
			m[t.dc]++
		}
		return m
	}

	t.Run("没超上限时一台不少", func(t *testing.T) {
		tasks := buildOrderTasks([]string{"gra", "rbx"}, 3, 6)
		if len(tasks) != 6 {
			t.Fatalf("期望 6 个任务，实际 %d", len(tasks))
		}
		for dc, n := range count(tasks) {
			if n != 3 {
				t.Errorf("机房 %s 期望 3 台，实际 %d", dc, n)
			}
		}
	})

	t.Run("超上限时按机房均分", func(t *testing.T) {
		// 5 个机房 × 3 台 = 15 单,封到 10
		dcs := []string{"gra", "rbx", "sbg", "bhs", "waw"}
		tasks := buildOrderTasks(dcs, 3, 10)
		if len(tasks) != 10 {
			t.Fatalf("期望截断到 10 个任务，实际 %d", len(tasks))
		}
		got := count(tasks)
		// 关键:每个机房都还有份额。旧的「机房在外、轮次在内」写法在这里
		// 会排成 gra×3, rbx×3, sbg×3, bhs×1 —— waw 一台都没有。
		for _, dc := range dcs {
			if got[dc] == 0 {
				t.Errorf("机房 %s 被整个砍掉了，均分失败：%v", dc, got)
			}
		}
		for _, dc := range dcs {
			if got[dc] > 3 {
				t.Errorf("机房 %s 分到 %d 台，超过 quantity=3", dc, got[dc])
			}
		}
	})

	t.Run("上限小于机房数时也不重复给同一个机房", func(t *testing.T) {
		tasks := buildOrderTasks([]string{"gra", "rbx", "sbg"}, 5, 2)
		if len(tasks) != 2 {
			t.Fatalf("期望 2 个任务，实际 %d", len(tasks))
		}
		if tasks[0].dc == tasks[1].dc {
			t.Errorf("上限紧张时应先铺开机房，实际两个任务都在 %s", tasks[0].dc)
		}
	})

	t.Run("退化输入返回空而不是 panic", func(t *testing.T) {
		for _, c := range []struct {
			name     string
			dcs      []string
			quantity int
			limit    int
		}{
			{"quantity=0", []string{"gra"}, 0, 10},
			{"quantity 负数", []string{"gra"}, -1, 10},
			{"limit=0", []string{"gra"}, 3, 0},
			{"limit 负数", []string{"gra"}, 3, -5},
			{"没有机房", nil, 3, 10},
		} {
			if got := buildOrderTasks(c.dcs, c.quantity, c.limit); len(got) != 0 {
				t.Errorf("%s：期望空任务列表，实际 %d 个", c.name, len(got))
			}
		}
	})
}

// idx 是给日志用的"这个机房的第几台",不能因为改了排列顺序就错位
func TestBuildOrderTasksIdx(t *testing.T) {
	tasks := buildOrderTasks([]string{"gra", "rbx"}, 2, 4)
	seen := map[string][]int{}
	for _, t := range tasks {
		seen[t.dc] = append(seen[t.dc], t.idx)
	}
	for dc, idxs := range seen {
		if len(idxs) != 2 || idxs[0] != 0 || idxs[1] != 1 {
			t.Errorf("机房 %s 的 idx 序列应为 [0 1]，实际 %v", dc, idxs)
		}
	}
}
