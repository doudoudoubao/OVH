package db

// 每个落库结构体的每个字段都填非零值,经**真实 SQLite** 写入再读回,
// 列出读回来和写进去不一样的字段。
//
// 为什么要这条:月费上限(MaxMonthly 等 5 个字段)曾经只加在了 Go 结构体上,
// 表里没列、INSERT 也没带,写入被静默丢弃 —— 重启后新机型自动下单变成不限价。
// 当时的"落库往返"测试只测了两个结构体之间的转换,从没经过数据库,所以一直是绿的。
// 反射逐字段比对的好处是:以后给这些结构体加字段,忘了同步表结构的那一刻就会变红。

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/ovh-buy/server/internal/types"
)

func fillNonZero(v reflect.Value, name string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x-" + name)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(7)
	case reflect.Float64, reflect.Float32:
		v.SetFloat(7.5)
	case reflect.Ptr:
		p := reflect.New(v.Type().Elem())
		fillNonZero(p.Elem(), name)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0), name)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		val := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(val, name)
		m.SetMapIndex(reflect.ValueOf("k"), val)
		v.Set(m)
	case reflect.Interface:
		v.Set(reflect.ValueOf("iv-" + name))
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillNonZero(v.Field(i), v.Type().Field(i).Name)
			}
		}
	}
}

func diffFields(t *testing.T, label string, want, got interface{}) {
	wv, gv := reflect.ValueOf(want), reflect.ValueOf(got)
	for i := 0; i < wv.NumField(); i++ {
		f := wv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if !reflect.DeepEqual(wv.Field(i).Interface(), gv.Field(i).Interface()) {
			t.Errorf("%s.%s 没扛过 SQLite 往返(表里缺列或 INSERT 漏了它): 写入 %s → 读回 %s", label, f.Name,
				short(wv.Field(i).Interface()), short(gv.Field(i).Interface()))
		}
	}
}

func short(v interface{}) string {
	s := fmt.Sprintf("%#v", v)
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}

func TestEveryFieldSurvivesSQLiteRoundTrip(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var q types.QueueItem
	fillNonZero(reflect.ValueOf(&q).Elem(), "q")
	if err := d.ReplaceQueue([]types.QueueItem{q}); err != nil {
		t.Fatal(err)
	}
	qs, _ := d.ListQueue()
	diffFields(t, "QueueItem", q, qs[0])

	var h types.PurchaseHistoryEntry
	fillNonZero(reflect.ValueOf(&h).Elem(), "h")
	if err := d.ReplaceHistory([]types.PurchaseHistoryEntry{h}); err != nil {
		t.Fatal(err)
	}
	hs, _ := d.ListHistory()
	diffFields(t, "PurchaseHistoryEntry", h, hs[0])

	var s types.Subscription
	fillNonZero(reflect.ValueOf(&s).Elem(), "s")
	if err := d.ReplaceMonitorSubscriptions([]types.Subscription{s}); err != nil {
		t.Fatal(err)
	}
	ss, _ := d.ListMonitorSubscriptions()
	diffFields(t, "Subscription", s, ss[0])

	var v types.VPSSubscription
	fillNonZero(reflect.ValueOf(&v).Elem(), "v")
	if err := d.ReplaceVPSSubscriptions([]types.VPSSubscription{v}); err != nil {
		t.Fatal(err)
	}
	vs, _ := d.ListVPSSubscriptions()
	diffFields(t, "VPSSubscription", v, vs[0])

	var a types.OVHAccount
	fillNonZero(reflect.ValueOf(&a).Elem(), "a")
	if err := d.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	as, _ := d.ListAccounts()
	diffFields(t, "OVHAccount", a, as[0])
}
