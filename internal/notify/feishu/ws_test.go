package feishu

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestLongConnManagerEnsure 验证凭据变化与长连接收敛之间的对应关系。
//
// 这里最要紧的是「凭据没变就不要重连」：设置页保存其它选项时也会走同一个入口，
// 若每次都重连，不光白白丢掉几秒的按钮回调，飞书那边还可能因为连接过频而拒绝。
//
// 断言只看代次（gen）与「明确停止」这两种确定的状态：真实建连是异步的，
// 凭据无效时会很快失败并把 running 置回 false，据此断言会变成偶发失败。
func TestLongConnManagerEnsure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewLongConnManager(ctx, nil, quietLogger())

	if m.Running() {
		t.Fatal("刚构造就不该是运行状态")
	}

	// 凭据不全：不启动，也不推进代次
	m.Ensure("", "")
	m.Ensure("cli_only_app_id", "")
	if m.Running() {
		t.Fatal("凭据不全时不该启动长连接")
	}
	if m.gen != 0 {
		t.Fatalf("凭据不全时不该推进代次，得到 gen=%d", m.gen)
	}

	// 完整凭据：拉起一次
	m.Ensure("cli_probe", "secret_probe")
	genAfterStart := m.gen
	if genAfterStart != 1 {
		t.Fatalf("首次给出完整凭据应启动一次，得到 gen=%d", genAfterStart)
	}

	// 重复喂同一份凭据：绝不能重连
	m.Ensure("cli_probe", "secret_probe")
	m.Ensure("cli_probe", "secret_probe")
	if m.gen != genAfterStart {
		t.Fatalf("凭据未变化却重连了：gen %d -> %d", genAfterStart, m.gen)
	}

	// 换 App Secret：重连一次
	m.Ensure("cli_probe", "secret_changed")
	if m.gen != genAfterStart+1 {
		t.Fatalf("凭据变化后应重连一次：gen %d -> %d", genAfterStart+1, m.gen)
	}

	// 换 App ID：同样重连一次
	m.Ensure("cli_another", "secret_changed")
	if m.gen != genAfterStart+2 {
		t.Fatalf("App ID 变化后应重连一次，得到 gen=%d", m.gen)
	}

	// 清空凭据：明确停止
	m.Ensure("", "")
	if m.Running() {
		t.Fatal("清空凭据后应停止长连接")
	}

	// Stop 幂等
	m.Stop()
	m.Stop()
	if m.Running() {
		t.Fatal("Stop 之后不该仍是运行状态")
	}
}
