package telegram

import (
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// Telegram 对 callback_data 有 64 字节硬限制，超了整条消息都发不出去
// （报 BUTTON_DATA_INVALID）。这里用最长的动作名 + 18 位 ID 兜一下底：
// 一旦有人往载荷里加字段，这条测试会立刻红。
func TestCallbackValueFitsLimit(t *testing.T) {
	longest := CallbackValue{Action: ActionSnooze, NID: 9223372036854775807}
	got := longest.Encode()
	if n := len(got); n > 64 {
		t.Fatalf("callback_data 超出 64 字节限制: %d 字节 (%q)", n, got)
	}

	back, err := DecodeCallback(got)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if back != longest {
		t.Fatalf("编解码不一致: %+v -> %q -> %+v", longest, got, back)
	}
}

// 测试按钮不带通知 ID，只靠动作名就要能解析出来。
func TestDecodeCallbackTestAction(t *testing.T) {
	v, err := DecodeCallback(CallbackValue{Action: ActionTest}.Encode())
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if v.Action != ActionTest || v.NID != 0 {
		t.Fatalf("解析结果不符: %+v", v)
	}
}

func TestDecodeCallbackRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "update", "update:abc", ":12"} {
		if _, err := DecodeCallback(bad); err == nil {
			t.Fatalf("%q 应被判为非法", bad)
		}
	}
}

// 更新消息必须带三个按钮，否则用户没有任何可点的操作；
// 正文里的 Markdown 片段要翻成 HTML，否则发出去会看到一堆反引号和星号。
func TestBuildUpdateMessage(t *testing.T) {
	n := &model.Notification{
		ID:            7,
		ImageRef:      "nginx:1.25",
		ContainerName: "web",
		Kind:          model.NotifNewTag,
		Title:         "web 有新版本可用",
		Body:          "当前版本 `1.25`，仓库中已存在更高版本 **1.27**",
		LocalTag:      "1.25",
		LatestTag:     "1.27",
	}

	m := BuildUpdateMessage(n)
	buttons := 0
	for _, row := range m.Rows {
		buttons += len(row)
	}
	if buttons != 3 {
		t.Fatalf("应有 3 个按钮，实际 %d 个", buttons)
	}
	if !strings.Contains(m.Text, "<code>1.25</code>") {
		t.Fatalf("正文里的行内代码应转成 <code>：%s", m.Text)
	}
	if !strings.Contains(m.Text, "<b>1.27</b>") {
		t.Fatalf("正文里的加粗应转成 <b>：%s", m.Text)
	}
	if strings.Contains(m.Text, "**") || strings.Contains(m.Text, "`") {
		t.Fatalf("正文里不应残留 Markdown 记号：%s", m.Text)
	}

	markup := m.markup()
	if markup == nil {
		t.Fatal("带按钮的消息必须生成 inline_keyboard")
	}
}

// 镜像名里带 < & 时若不转义，Telegram 会判定整条消息格式错误、直接发不出去。
func TestBuildUpdateMessageEscapesHTML(t *testing.T) {
	n := &model.Notification{
		ID:            1,
		ImageRef:      "a<b&c:d",
		ContainerName: "x<y",
		Title:         "t&t",
		Body:          "b",
	}
	m := BuildUpdateMessage(n)
	if strings.Contains(m.Text, "<b&") || strings.Contains(m.Text, "x<y") {
		t.Fatalf("HTML 特殊字符未转义：%s", m.Text)
	}
}

// 数据源改了之后，由数据源决定解析：scanner 已经按 >0 check 过了。这里只验按钮。
func TestBuildStateMessageRetryButton(t *testing.T) {
	n := &model.Notification{ID: 3, ImageRef: "nginx:latest", ContainerName: "web"}

	failed := BuildStateMessage(n, model.CardFailed, "拉取超时")
	rows := failed.Rows
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("失败态应有一个「重试更新」按钮，实际 %+v", rows)
	}
	if rows[0][0].Data != "update:3" {
		t.Fatalf("重试按钮的载荷应是 update:3，实际 %q", rows[0][0].Data)
	}

	done := BuildStateMessage(n, model.CardDone, "完成")
	if len(done.Rows) != 0 {
		t.Fatalf("成功态不该再留按钮，实际 %+v", done.Rows)
	}
}
