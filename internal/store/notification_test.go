package store

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 两个渠道同时配好时，一条提醒会各发一份，两份消息 ID 互不相干。
// 投递列表必须能原样存回去 —— 少一条，那个渠道的卡片更新完也改不动。
func TestNotificationDeliveriesRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, err := s.CreateNotification(&model.Notification{
		ImageRef: "demo/app:1.0",
		Kind:     model.NotifNewVersion,
		Title:    "t",
	})
	if err != nil {
		t.Fatalf("写通知失败: %v", err)
	}

	list := []model.Delivery{
		{Channel: model.ChannelFeishu, ChatRef: "oc_x", MessageID: "om_1"},
		{Channel: model.ChannelTelegram, ChatRef: "-100123", MessageID: "456"},
	}
	if err := s.SetNotificationDeliveries(id, list, model.CardPending); err != nil {
		t.Fatalf("写投递记录失败: %v", err)
	}

	got, err := s.GetNotification(id)
	if err != nil {
		t.Fatalf("读通知失败: %v", err)
	}
	if len(got.Deliveries) != 2 {
		t.Fatalf("应存下 2 条投递，得到 %d 条: %+v", len(got.Deliveries), got.Deliveries)
	}
	if got.Deliveries[1].Channel != model.ChannelTelegram || got.Deliveries[1].MessageID != "456" {
		t.Fatalf("Telegram 那条投递没存对: %+v", got.Deliveries[1])
	}
	// 主投递取第一条，给前端展示与老代码用
	if got.Channel != model.ChannelFeishu || got.MessageID != "om_1" {
		t.Fatalf("主投递应取列表第一条，得到 %q / %q", got.Channel, got.MessageID)
	}
}

// 纠正 Telegram 会话时只能改 Telegram 那一份，别把飞书的会话 ID 顺手覆盖掉。
func TestUpdateNotificationDeliveriesKeepsOthers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, _ := s.CreateNotification(&model.Notification{ImageRef: "demo/app:1.0", Kind: model.NotifNewVersion})
	list := []model.Delivery{
		{Channel: model.ChannelFeishu, ChatRef: "oc_x", MessageID: "om_1"},
		{Channel: model.ChannelTelegram, ChatRef: "-100123", MessageID: "456"},
	}
	if err := s.SetNotificationDeliveries(id, list, model.CardPending); err != nil {
		t.Fatalf("写投递记录失败: %v", err)
	}

	list[1].ChatRef = "-100999"
	if err := s.UpdateNotificationDeliveries(id, list); err != nil {
		t.Fatalf("改投递记录失败: %v", err)
	}

	got, _ := s.GetNotification(id)
	if got.Deliveries[1].ChatRef != "-100999" {
		t.Fatalf("Telegram 会话没改过来: %+v", got.Deliveries[1])
	}
	if got.Deliveries[0].ChatRef != "oc_x" {
		t.Fatalf("飞书那条被误改了: %+v", got.Deliveries[0])
	}
}

// 老库（1.2.4 之前）没有 deliveries 列的内容，读出来应为空而不是报错。
func TestNotificationWithoutDeliveriesIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, _ := s.CreateNotification(&model.Notification{ImageRef: "demo/app:1.0", Kind: model.NotifNewVersion})
	if _, err := s.db.Exec(`UPDATE notifications SET deliveries = '' WHERE id = ?`, id); err != nil {
		t.Fatalf("构造老数据失败: %v", err)
	}

	got, err := s.GetNotification(id)
	if err != nil {
		t.Fatalf("读老记录失败: %v", err)
	}
	if len(got.Deliveries) != 0 {
		t.Fatalf("老记录不应有投递列表，得到 %+v", got.Deliveries)
	}
}
