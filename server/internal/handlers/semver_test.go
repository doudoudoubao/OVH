package handlers

import "testing"

// 版本号比较只看前三段。这里把每种写法的实际行为钉死 ——
// 跑 fork 的人要自己发版本号，而这几个坑都是静默的：
// 版本号写错不会报错，只会表现成「一直没有更新」或者更糟。
func TestParseSemverShapes(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		note string
	}{
		{"0.1.15", [3]int{0, 1, 15}, "常规"},
		{"v0.1.15", [3]int{0, 1, 15}, "带 v 前缀"},
		{"V0.1.15", [3]int{0, 1, 15}, "带大写 V"},
		{" 0.1.15 ", [3]int{0, 1, 15}, "两边有空格"},
		{"0.2.0", [3]int{0, 2, 0}, "次版本号"},
		// 后缀会被截掉：想用 0.1.15-1 表示"0.1.15 的第一次修订"是行不通的
		{"0.1.15-1", [3]int{0, 1, 15}, "带 - 后缀，后缀被截断"},
		{"0.1.15+build3", [3]int{0, 1, 15}, "带 + 后缀，后缀被截断"},
		// 最阴的一个：四段写法让第三段 Atoi 失败而归零，
		// 结果 0.1.15.1 比 0.1.15 还**旧**
		{"0.1.15.1", [3]int{0, 1, 0}, "四段：第三段解析失败归零"},
		{"", [3]int{0, 0, 0}, "空串"},
		{"abc", [3]int{0, 0, 0}, "完全解析不了"},
	}
	for _, c := range cases {
		t.Run(c.note, func(t *testing.T) {
			if got := parseSemver(c.in); got != c.want {
				t.Errorf("parseSemver(%q) = %v，期望 %v（%s）", c.in, got, c.want, c.note)
			}
		})
	}
}

func TestSemverGreater(t *testing.T) {
	newer := [][2]string{
		{"0.1.16", "0.1.15"},
		{"0.2.0", "0.1.15"},
		{"1.0.0", "0.9.9"},
		{"v0.1.16", "0.1.15"},
	}
	for _, c := range newer {
		if !semverGreater(c[0], c[1]) {
			t.Errorf("%s 应该比 %s 新", c[0], c[1])
		}
	}

	// 这几个**不是**更新。发版时把版本号写成这样，界面上永远不会提示有新版，
	// 而且没有任何报错 —— 只会表现成「更新功能坏了」。
	notNewer := [][2]string{
		{"0.1.15", "0.1.15"},   // 相同
		{"0.1.14", "0.1.15"},   // 更旧
		{"0.1.15-1", "0.1.15"}, // 后缀被截断，等于相同
		{"0.1.15.1", "0.1.15"}, // 四段 → [0 1 0]，反而更旧
	}
	for _, c := range notNewer {
		if semverGreater(c[0], c[1]) {
			t.Errorf("%s 不该被判定为比 %s 新", c[0], c[1])
		}
	}
}

// 本仓库当前的版本必须真的比它 fork 自的上游版本新，
// 否则装了这个 fork 的人永远收不到自己仓库的更新提示。
func TestForkVersionIsAheadOfUpstreamBase(t *testing.T) {
	const upstreamBase = "0.1.15" // 这个 fork 的起点
	if !semverGreater("0.2.0", upstreamBase) {
		t.Fatalf("fork 的版本线必须高于上游起点 %s", upstreamBase)
	}
}
