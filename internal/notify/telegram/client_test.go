package telegram

import (
	"net/http"
	"strings"
	"testing"
)

// 配了代理就必须真的走代理 —— 国内直连 api.telegram.org 基本不通，
// 这条路径是唯一的活路，静默失效等于功能不存在。
func TestNewHTTPClientUsesProxy(t *testing.T) {
	const raw = "http://192.168.1.2:7890"
	c := newHTTPClient(raw)

	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("Transport 上没有配置 Proxy")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot123/getMe", nil)
	u, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("取代理地址失败: %v", err)
	}
	if u == nil || u.Host != "192.168.1.2:7890" {
		t.Fatalf("代理地址不对：%v", u)
	}
	if u.Scheme != "http" {
		t.Errorf("协议不对：%s", u.Scheme)
	}
}

// 留空时不能落成「空代理」——那会让所有请求去连一个不存在的代理，比直连还糟。
//
// 这里不假设「一定没有代理」：环境变量里配了 HTTPS_PROXY 的机器本来就会走代理，
// 断言的是「没被硬塞成某个地址」，而不是「一定直连」。
func TestNewHTTPClientNoProxy(t *testing.T) {
	c := newHTTPClient("")
	tr := c.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("没配代理时应保留环境变量代理能力（ProxyFromEnvironment）")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot123/getMe", nil)
	if u, _ := tr.Proxy(req); u != nil && u.Host == "192.168.1.2:7890" {
		t.Fatal("留空时不该套上任何硬编码代理")
	}
}

// 代理地址照代理软件界面的写法敲（不带协议）是最常见的情形。
// 自动补 http:// 不是体贴，是必需：url.Parse 对「192.168.1.2:7890」直接报错，
// 早先的实现会把错吞掉退回直连，用户看到的现象是「填了代理还是不通」。
func TestParseProxy(t *testing.T) {
	cases := []struct {
		in     string
		scheme string
		host   string
		drop   bool // true 表示解析失败、应退回直连
	}{
		{in: "http://192.168.1.2:7890", scheme: "http", host: "192.168.1.2:7890"},
		{in: "192.168.1.2:7890", scheme: "http", host: "192.168.1.2:7890"},
		{in: "  10.0.0.9:1080 ", scheme: "http", host: "10.0.0.9:1080"},
		{in: "socks5://127.0.0.1:7891", scheme: "socks5", host: "127.0.0.1:7891"},
		{in: "http://user:pass@192.168.1.2:7890", scheme: "http", host: "192.168.1.2:7890"},
		{in: "", drop: true},
		{in: "   ", drop: true},
		{in: "http://", drop: true},
	}
	for _, c := range cases {
		u := parseProxy(c.in)
		if c.drop {
			if u != nil {
				t.Errorf("%q 应解析失败，却得到 %v", c.in, u)
			}
			continue
		}
		if u == nil {
			t.Errorf("%q 应该能解析出来", c.in)
			continue
		}
		if u.Scheme != c.scheme || u.Host != c.host {
			t.Errorf("%q 解析成 %s://%s，期望 %s://%s", c.in, u.Scheme, u.Host, c.scheme, c.host)
		}
	}
}

// 错误文本会直接显示在设置页上，用户截图求助是常态。
// 早先网络层错误的原文带着完整请求 URL，等于把 bot token 一起截图发出去 ——
// 拿到它就能冒充这个 bot。这里钉死「只留数字 ID，字母全抹」。
func TestRedactToken(t *testing.T) {
	const token = "123456789:AAHsecretsecretsecretsecretsecret"
	msg := `请求 Telegram getMe 失败: Post "https://api.telegram.org/bot123456789:AAHsecretsecretsecretsecretsecret/getMe": dial tcp: timeout`

	got := redactToken(msg, token)
	if strings.Contains(got, "AAHsecret") {
		t.Fatalf("token 没被抹掉：%s", got)
	}
	if !strings.Contains(got, "123456789:***") {
		t.Fatalf("应保留数字 ID 便于对号：%s", got)
	}
	// 其余信息必须原样留着，否则报错就没用了
	if !strings.Contains(got, "dial tcp: timeout") || !strings.Contains(got, "getMe") {
		t.Fatalf("把有用信息一起抹掉了：%s", got)
	}

	// token 为空时不能把整段文本吃掉
	if redactToken("hello", "") != "hello" {
		t.Fatal("空 token 不该改动文本")
	}
}
