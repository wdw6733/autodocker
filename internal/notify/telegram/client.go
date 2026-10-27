// Package telegram 封装 Telegram Bot API：发消息、带按钮的交互消息、
// 以及用长轮询接收按钮点击。
//
// 为什么用长轮询而不是 Webhook：Webhook 要求一个公网可达的 HTTPS 地址
// （域名 + 证书 + 反代），而这套服务的部署场景是家里/内网的 NAS。
// getUpdates 由本服务主动向 Telegram 拉取，同样是「即点即达」，
// 但不需要任何入站端口 —— 与飞书那边的长连接是同一个思路。
//
// 限制：同一个 bot 的 getUpdates 与 setWebhook 互斥，这里只走前者。
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultAPIBase Telegram 官方接口地址。
const DefaultAPIBase = "https://api.telegram.org"

// Config bot 配置。
type Config struct {
	Token   string // BotFather 给的 token，形如 123456:AA...
	ChatID  string // 默认接收会话：私聊是正数，群/频道是负数（-100…）
	APIBase string // 默认 https://api.telegram.org；国内可换成自建反代
	Proxy   string // HTTP 代理，形如 http://192.168.1.2:7890；留空直连
}

// Client Telegram Bot API 客户端。
type Client struct {
	mu  sync.RWMutex
	cfg Config
	hc  *http.Client
}

// New 构造客户端。
func New(cfg Config) *Client {
	cfg = normalize(cfg)
	return &Client{cfg: cfg, hc: newHTTPClient(cfg.Proxy)}
}

// newHTTPClient 按代理设置构造 HTTP 客户端。
//
// 国内大部分网络直连 api.telegram.org 是不通的（表现为 TLS 握手超时），
// 而 NAS 上往往已经有 Clash 之类的代理在跑，所以给一个「填代理地址」的口子，
// 比逼用户自己搭反代现实得多。写错的地址退回直连，不让服务起不来。
func newHTTPClient(proxy string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if u := parseProxy(proxy); u != nil {
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: 70 * time.Second, Transport: tr}
}

// parseProxy 解析代理地址，解析不出来返回 nil（调用方据此退回直连）。
//
// 没写协议的自动补 http:// —— 人人都是照着代理软件的界面敲「192.168.1.2:7890」，
// 而 url.Parse 对不带协议的串直接报错（首段含冒号），不补就静默变成直连，
// 表现成「填了代理还是不通」，最难查。
// socks5:// 也认：标准库的 Transport 原生支持，Clash 的 socks 端口能直接用。
func parseProxy(s string) *url.URL {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}

func normalize(cfg Config) Config {
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.ChatID = strings.TrimSpace(cfg.ChatID)
	cfg.APIBase = strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	cfg.Proxy = strings.TrimSpace(cfg.Proxy)
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	return cfg
}

// Configured 是否填了 token。有 token 就能收按钮回调，chat_id 只影响往哪发。
func (c *Client) Configured() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Token != ""
}

// DefaultChatID 返回默认接收会话。
func (c *Client) DefaultChatID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.ChatID
}

// APIBase 当前使用的接口地址，供设置页显示排查信息。
func (c *Client) APIBase() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.APIBase
}

// Proxy 当前使用的代理地址，未配置时为空。
func (c *Client) Proxy() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Proxy
}

// Token 当前使用的 Bot Token（仅供后台自己复用配置，不要往响应里带）。
func (c *Client) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Token
}

// SetOptions 更新配置（换代理要重建 Transport，所以整份重来）。
func (c *Client) SetOptions(cfg Config) {
	cfg = normalize(cfg)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.hc = newHTTPClient(cfg.Proxy)
}

// snapshot 取一份配置副本，避免持锁做网络请求。
func (c *Client) snapshot() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// httpClient 取当前的 HTTP 客户端。换代理时 SetOptions 会整个换掉它，
// 所以调用方必须在发起请求前拿一次，不能反复读字段。
func (c *Client) httpClient() *http.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hc
}

// ---- HTTP 基础设施 ----

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

// call 发起一次 Bot API 调用。token 从 URL 里取，不放请求头。
//
// 返回前统一把错误文本里的 token 抹掉：网络层错误的原文带着完整请求 URL
// （`.../bot123456:AAH…/getMe`），而这段文本会直接显示在设置页上 ——
// 用户一截图求助，token 就等于公开了，拿到它就能冒充这个 bot 收发消息。
func (c *Client) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	cfg := c.snapshot()
	data, err := c.do(ctx, cfg, method, payload)
	if err != nil {
		return nil, errors.New(redactToken(err.Error(), cfg.Token))
	}
	return data, nil
}

// redactToken 把文本里出现的 token 换成「数字:***」。
//
// 只保留冒号前的数字 ID 便于对号（哪个 bot），字母部分一律抹掉 ——
// 那才是能拿来冒充的凭据。
func redactToken(s, token string) string {
	if s == "" || token == "" {
		return s
	}
	masked := "***"
	if i := strings.IndexByte(token, ':'); i > 0 {
		masked = token[:i] + ":***"
	}
	return strings.ReplaceAll(s, token, masked)
}

func (c *Client) do(ctx context.Context, cfg Config, method string, payload any) (json.RawMessage, error) {
	if cfg.Token == "" {
		return nil, errors.New("Telegram Bot Token 未配置")
	}

	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(buf)
	}
	url := fmt.Sprintf("%s/bot%s/%s", cfg.APIBase, cfg.Token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// 网络层错误最常见的是「连不上 api.telegram.org」，
		// 把方法名带上，用户才知道是哪一步没通。
		return nil, fmt.Errorf("请求 Telegram %s 失败: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 Telegram 响应失败: %w (原始响应: %s)", err, truncate(string(raw), 300))
	}
	if !out.OK {
		return nil, fmt.Errorf("Telegram 返回错误: %d %s", out.ErrorCode, out.Description)
	}
	return out.Result, nil
}

// GetMe 校验 token 是否有效，返回 bot 用户名。
func (c *Client) GetMe(ctx context.Context) (string, error) {
	data, err := c.call(ctx, "getMe", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("解析 getMe 结果失败: %w", err)
	}
	return out.Username, nil
}

// SendMessage 发送一条消息（可带内联按钮），返回 message_id。
func (c *Client) SendMessage(ctx context.Context, chatID string, m Message) (int64, error) {
	if chatID == "" {
		chatID = c.DefaultChatID()
	}
	if chatID == "" {
		return 0, errors.New("未配置接收会话 chat_id")
	}
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     m.Text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if markup := m.markup(); markup != nil {
		payload["reply_markup"] = markup
	}
	data, err := c.call(ctx, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	var out struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("解析发送结果失败: %w", err)
	}
	return out.MessageID, nil
}

// EditMessageText 改写已发送的消息。按钮点击后把原消息替换成结果态，
// 与飞书的「原地换卡片」等价，形成一条可追溯的操作记录。
func (c *Client) EditMessageText(ctx context.Context, chatID string, messageID int64, m Message) error {
	if chatID == "" {
		chatID = c.DefaultChatID()
	}
	if chatID == "" || messageID == 0 {
		return errors.New("缺少 chat_id 或 message_id")
	}
	payload := map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     m.Text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	// 显式发空键盘，把原来那排按钮摘掉 —— 否则点完还留着可点的旧按钮，
	// 用户会以为没生效、反复点。
	payload["reply_markup"] = map[string]any{"inline_keyboard": []any{}}
	if markup := m.markup(); markup != nil {
		payload["reply_markup"] = markup
	}
	_, err := c.call(ctx, "editMessageText", payload)
	return err
}

// AnswerCallbackQuery 回应一次按钮点击。
// 不回的话客户端按钮会一直转圈，用户以为卡住了。
func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	if callbackID == "" {
		return nil
	}
	_, err := c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	})
	return err
}

// Update 一次 getUpdates 返回的更新项，只保留用得上的字段。
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *ChatMessage   `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// ChatMessage 普通消息，用来发现「bot 被拉进了哪个会话」。
type ChatMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      Chat  `json:"chat"`
}

// CallbackQuery 按钮点击。
type CallbackQuery struct {
	ID      string       `json:"id"`
	Data    string       `json:"data"`
	Message *ChatMessage `json:"message"`
	From    *User        `json:"from"`
}

// Chat 会话信息。
type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // private / group / supergroup / channel
	Title string `json:"title"`
}

// User 发起人。
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// GetUpdates 拉取待处理的更新。timeout 为长轮询等待秒数（0 表示立即返回）。
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}
	// 长轮询的等待时间要留在 HTTP 超时之内，否则这边先断，永远拿不到结果
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+15)*time.Second)
	defer cancel()
	data, err := c.call(callCtx, "getUpdates", payload)
	if err != nil {
		return nil, err
	}
	var out []Update
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析 getUpdates 结果失败: %w", err)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
