package telegram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBot 起一个只应答 getUpdates 的假 Telegram。
//
// 第一次调用（offset=-1，轮询启动时那次）返回 backlog，之后的调用一律返回空 ——
// 正是真实 getUpdates 的行为：offset 一旦推进，旧更新就再也拿不到了。
func fakeBot(t *testing.T, backlog string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = io.WriteString(w, `{"ok":true,"result":`+backlog+`}`)
			return
		}
		// 后续轮询故意慢一点，避免测试里空转把 CPU 拉满
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

type countingHandler struct{ calls int32 }

func (h *countingHandler) HandleCallback(_ context.Context, _ CallbackValue, _ string, _ int64) (string, *Message) {
	atomic.AddInt32(&h.calls, 1)
	return "", nil
}

func waitChats(t *testing.T, m *PollManager, want int) []ChatInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := m.Chats(); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return m.Chats()
}

// 轮询启动前就在群里说过的那句话，必须能被「读取会话」看到。
//
// 这里的顺序是真实用户最常干的事：先在群里发话、再回来填 Token。
// 那些消息躺在 backlog 里，初始化时必须记下会话，否则列表永远空着，
// 提示词还一个劲让人「再发一句话」——排查方向直接被带偏。
func TestPollManagerRecordsBacklogChat(t *testing.T) {
	backlog := `[{"update_id":7,"message":{"message_id":1,
		"chat":{"id":-1001234567890,"type":"supergroup","title":"各种通知"},"text":"hi"}}]`
	srv, _ := fakeBot(t, backlog)

	m := NewPollManager(context.Background(), &countingHandler{}, discardLogger())
	m.Ensure(Config{Token: "123:abc", APIBase: srv.URL}) // chat_id 留空：正是「还没读到会话」的状态
	defer m.Stop()

	chats := waitChats(t, m, 1)
	if len(chats) != 1 {
		t.Fatalf("积压里的消息没有被记进会话列表，得到 %d 条", len(chats))
	}
	if chats[0].ID != -1001234567890 {
		t.Errorf("会话 ID 不对：%d", chats[0].ID)
	}
	if chats[0].Title != "各种通知" {
		t.Errorf("会话标题不对：%q", chats[0].Title)
	}
}

// 积压里的按钮点击绝不能被执行：那可能是停机期间别人点的，
// 对应的容器早就被手动处理过了。这条是「初始化只消费、不执行」的护栏。
func TestPollManagerIgnoresBacklogCallback(t *testing.T) {
	backlog := `[{"update_id":9,"callback_query":{"id":"cb1","data":"act=update&id=42",
		"message":{"message_id":3,"chat":{"id":-1001234567890,"type":"supergroup","title":"群"}}}}]`
	srv, _ := fakeBot(t, backlog)

	h := &countingHandler{}
	m := NewPollManager(context.Background(), h, discardLogger())
	m.Ensure(Config{Token: "123:abc", ChatID: "-1001234567890", APIBase: srv.URL})
	defer m.Stop()

	// 给「万一要执行」留出时间窗，再确认回调一次都没进来
	time.Sleep(250 * time.Millisecond)
	if n := atomic.LoadInt32(&h.calls); n != 0 {
		t.Fatalf("积压的按钮点击被执行了 %d 次", n)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
