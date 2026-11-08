package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/notify/feishu"
	"github.com/haihaipypy/autodocker/internal/notify/telegram"
)

// quietService 造一个只带 logger 的 Service，刻意不装配 store / updater / 渠道客户端。
//
// 这是有意为之：测试消息上的回调不关联任何通知，必须在碰数据库**之前**就被受理。
// 一旦有人把 ActionTest 分支挪到 GetNotification 后面，下面这些测试立刻会因为
// nil store 而 panic —— 正好把这类回归卡死在提交前。
func quietService() *Service {
	return &Service{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestHandleCardActionAcceptsTestCallback(t *testing.T) {
	s := quietService()

	toast, card := s.HandleCardAction(context.Background(), feishu.CardValue{Action: feishu.ActionTest})
	if toast == "" {
		t.Fatal("测试回调应返回一句 toast 反馈，否则群里点了没有任何提示")
	}
	if card == nil {
		t.Fatal("测试回调应返回回执卡片，让群里看到这一下点击真的生效了")
	}

	at, action, ch, count := s.CallbackState()
	if count != 1 {
		t.Fatalf("回调计数应为 1，得到 %d", count)
	}
	if action != feishu.ActionTest {
		t.Fatalf("记录的 action 应为 %q，得到 %q", feishu.ActionTest, action)
	}
	if ch != model.ChannelFeishu {
		t.Fatalf("应记录为飞书渠道，得到 %q", ch)
	}
	if at.IsZero() || time.Since(at) > time.Minute {
		t.Fatalf("回调时间戳不合理: %v", at)
	}
}

// Telegram 的测试按钮走另一条回调路径，同样必须在查库之前受理。
func TestHandleCallbackAcceptsTelegramTest(t *testing.T) {
	s := quietService()

	toast, msg := s.HandleCallback(context.Background(), telegram.CallbackValue{Action: telegram.ActionTest}, "123", 45)
	if toast == "" {
		t.Fatal("Telegram 测试回调应返回 toast，否则客户端按钮会一直转圈")
	}
	if msg == nil {
		t.Fatal("Telegram 测试回调应返回回执消息，原地替换掉带按钮的那条")
	}

	at, action, ch, count := s.CallbackState()
	if count != 1 || action != telegram.ActionTest || ch != model.ChannelTelegram {
		t.Fatalf("回调状态不符：count=%d action=%q channel=%q", count, action, ch)
	}
	if at.IsZero() {
		t.Fatal("回调时间戳不应为零值")
	}
}

// 设置页靠「有没有回调时间」区分「从未验证过」与「已验证」，初始必须是零值。
func TestCallbackStateZeroBeforeAnyCallback(t *testing.T) {
	at, action, ch, count := quietService().CallbackState()
	if !at.IsZero() || action != "" || ch != "" || count != 0 {
		t.Fatalf("初始状态应为零值，得到 %v / %q / %q / %d", at, action, ch, count)
	}
}

// 三个渠道的动作字符串必须完全一致：按钮里存的、落地时判断的，是同一批字面量。
// 任何一处改名却没同步，都会表现成「点了按钮提示未知操作」。
func TestActionConstantsAligned(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"update", ActionUpdate, feishu.ActionUpdate},
		{"ignore", ActionIgnore, feishu.ActionIgnore},
		{"snooze", ActionSnooze, feishu.ActionSnooze},
		{"test", ActionTest, feishu.ActionTest},
	}
	for _, p := range pairs {
		if p.a != p.b {
			t.Fatalf("%s 动作常量与飞书不一致: %q vs %q", p.name, p.a, p.b)
		}
	}
	tgPairs := []struct{ name, a, b string }{
		{"update", ActionUpdate, telegram.ActionUpdate},
		{"ignore", ActionIgnore, telegram.ActionIgnore},
		{"snooze", ActionSnooze, telegram.ActionSnooze},
		{"test", ActionTest, telegram.ActionTest},
	}
	for _, p := range tgPairs {
		if p.a != p.b {
			t.Fatalf("%s 动作常量与 Telegram 不一致: %q vs %q", p.name, p.a, p.b)
		}
	}
}

// 显式选了某个渠道、但它没配好时，不能偷偷换成另一个 —— 那会让用户
// 明明选了 Telegram 却收到飞书消息，比收不到更让人困惑。
func TestEffectiveChannelRespectsSelection(t *testing.T) {
	s := quietService()
	s.fs = feishu.New(feishu.Config{AppID: "cli_x", AppSecret: "secret"})
	s.tg = telegram.New(telegram.Config{})

	s.ApplyChannel(string(model.ChannelTelegram))
	if got := s.EffectiveChannel(); got != "" {
		t.Fatalf("选了 Telegram 但没填 token 时应为空，得到 %q", got)
	}

	s.ApplyChannel(string(model.ChannelFeishu))
	if got := s.EffectiveChannel(); got != model.ChannelFeishu {
		t.Fatalf("飞书已配好且被选中时应返回 feishu，得到 %q", got)
	}

	// 未指定渠道时沿用历史行为：飞书优先
	s.ApplyChannel("")
	if got := s.EffectiveChannel(); got != model.ChannelFeishu {
		t.Fatalf("未指定渠道时应回落到飞书，得到 %q", got)
	}
}

// 两个渠道都填全了就应该都算「就绪」，一条提醒两边都发。
// 用户把两个渠道都配好了，只发其中一个会让人以为另一个坏掉了。
func TestReadyChannelsIncludesBoth(t *testing.T) {
	s := quietService()
	s.fs = feishu.New(feishu.Config{AppID: "cli_x", AppSecret: "secret", ChatID: "oc_x"})
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123"})

	got := s.readyChannels()
	if len(got) != 2 || got[0] != model.ChannelFeishu || got[1] != model.ChannelTelegram {
		t.Fatalf("两个渠道都配好时应都就绪（飞书在前），得到 %v", got)
	}
}

// 凭据填了但没填接收会话，消息发不到任何地方，不能算就绪。
func TestReadyChannelsSkipsEmptyChatID(t *testing.T) {
	s := quietService()
	s.fs = feishu.New(feishu.Config{AppID: "cli_x", AppSecret: "secret"}) // 没有 ChatID
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123"})

	got := s.readyChannels()
	if len(got) != 1 || got[0] != model.ChannelTelegram {
		t.Fatalf("飞书缺 chat_id 时不应就绪，得到 %v", got)
	}
}

// Notify 要把投递坐标填进 Deliveries，而不是只回一个主投递。
// 用假的 Bot API 顶替真接口，验证「发出去」和「记下来」是同一次调用完成的。
func TestNotifyRecordsDelivery(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			hits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":456,"date":1700000000}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	s := quietService()
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123", APIBase: srv.URL})
	s.fs = feishu.New(feishu.Config{}) // 未配置，不参与

	n := &model.Notification{ImageRef: "demo/app:1.0", LatestTag: "1.1", Kind: model.NotifNewTag}
	msgID, err := s.Notify(context.Background(), n)
	if err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	if hits != 1 {
		t.Fatalf("应调用一次 sendMessage，实际 %d 次", hits)
	}
	if msgID != "456" {
		t.Fatalf("返回的消息 ID 应为 456，得到 %q", msgID)
	}
	if len(n.Deliveries) != 1 || n.Deliveries[0].Channel != model.ChannelTelegram {
		t.Fatalf("投递记录不对: %+v", n.Deliveries)
	}
	if n.Deliveries[0].ChatRef != "-100123" || n.Deliveries[0].MessageID != "456" {
		t.Fatalf("投递坐标不对: %+v", n.Deliveries[0])
	}
	if n.Channel != model.ChannelTelegram || n.ChatRef != "-100123" {
		t.Fatalf("主投递应同步为 Telegram，得到 %q / %q", n.Channel, n.ChatRef)
	}
}

// 一个渠道挂了不能连累另一个：两边都配了却只收到一条，也比一条都没收到强。
func TestNotifySurvivesOneChannelFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"description":"boom"}`))
	}))
	defer srv.Close()

	s := quietService()
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123", APIBase: srv.URL})
	s.fs = feishu.New(feishu.Config{}) // 未配置

	n := &model.Notification{ImageRef: "demo/app:1.0", LatestTag: "1.1", Kind: model.NotifNewTag}
	if _, err := s.Notify(context.Background(), n); err == nil {
		t.Fatal("唯一渠道失败时应返回错误，否则扫描器会把失败当成功记进日志")
	}
	if len(n.Deliveries) != 0 {
		t.Fatalf("失败时不应产生投递记录，得到 %+v", n.Deliveries)
	}
}

// 老库的通知没有 Deliveries，更新完成后仍要能改得动那唯一一条消息。
func TestDeliveryTargetsFallsBackToPrimary(t *testing.T) {
	s := quietService()

	old := &model.Notification{Channel: model.ChannelFeishu, ChatRef: "oc_x", MessageID: "om_1"}
	got := s.deliveryTargets(old)
	if len(got) != 1 || got[0].MessageID != "om_1" || got[0].Channel != model.ChannelFeishu {
		t.Fatalf("老数据应退回主投递，得到 %+v", got)
	}

	none := &model.Notification{}
	if got := s.deliveryTargets(none); got != nil {
		t.Fatalf("既无投递列表也无主投递时应返回 nil，得到 %+v", got)
	}
}
