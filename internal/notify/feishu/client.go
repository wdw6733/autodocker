// Package feishu 封装飞书自建应用的开放接口调用与卡片交互。
//
// 说明：真正能「按钮回调」的是**企业自建应用机器人**，
// 而不是群自定义机器人（webhook 只能单向发消息，收不到点击）。
// 因此这里使用 App ID / App Secret 换取 tenant_access_token 后走 OpenAPI。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config 飞书应用配置。
type Config struct {
	AppID     string
	AppSecret string
	Domain    string // 默认 https://open.feishu.cn
	ChatID    string // 默认接收群
}

// Client 飞书 OpenAPI 客户端，自动维护 tenant_access_token。
type Client struct {
	cfg Config
	hc  *http.Client

	mu       sync.Mutex
	token    string
	expireAt time.Time
}

// New 构造客户端。
func New(cfg Config) *Client {
	if cfg.Domain == "" {
		cfg.Domain = "https://open.feishu.cn"
	}
	cfg.Domain = strings.TrimRight(cfg.Domain, "/")
	return &Client{
		cfg: cfg,
		hc:  &http.Client{Timeout: 20 * time.Second},
	}
}

// Configured 是否已具备调用条件。
func (c *Client) Configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.AppID != "" && c.cfg.AppSecret != ""
}

// Credentials 返回当前应用凭据，供设置页回填。
func (c *Client) Credentials() (appID, appSecret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.AppID, c.cfg.AppSecret
}

// SetCredentials 更新应用凭据。
//
// 凭据变化时一并丢弃缓存的 tenant_access_token —— 否则换应用之后
// 还会拿着旧应用的 token 去发消息，报的错跟配置毫无关系，极难排查。
func (c *Client) SetCredentials(appID, appSecret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg.AppID == appID && c.cfg.AppSecret == appSecret {
		return
	}
	c.cfg.AppID = appID
	c.cfg.AppSecret = appSecret
	c.token = ""
	c.expireAt = time.Time{}
}

// DefaultChatID 返回默认接收群。
func (c *Client) DefaultChatID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.ChatID
}

// SetChatID 更新默认接收群。
func (c *Client) SetChatID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.ChatID = id
}

// ---- 内部 HTTP 基础设施 ----

type apiResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Token 获取（必要时刷新）tenant_access_token。
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.expireAt.Add(-2*time.Minute)) {
		tok := c.token
		c.mu.Unlock()
		return tok, nil
	}
	cfg := c.cfg
	c.mu.Unlock()

	if cfg.AppID == "" || cfg.AppSecret == "" {
		return "", errors.New("飞书应用未配置：缺少 App ID 或 App Secret")
	}

	body, err := json.Marshal(map[string]string{
		"app_id":     cfg.AppID,
		"app_secret": cfg.AppSecret,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.Domain+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求飞书 token 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析飞书 token 响应失败: %w", err)
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", fmt.Errorf("获取飞书 token 失败: code=%d msg=%s", out.Code, out.Msg)
	}

	expire := out.Expire
	if expire <= 0 {
		expire = 7200
	}
	c.mu.Lock()
	c.token = out.TenantAccessToken
	c.expireAt = time.Now().Add(time.Duration(expire) * time.Second)
	c.mu.Unlock()
	return out.TenantAccessToken, nil
}

// call 发起带鉴权的 OpenAPI 调用。
func (c *Client) call(ctx context.Context, method, path string, payload any) (json.RawMessage, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Domain+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用飞书接口失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析飞书响应失败: %w (原始响应: %s)", err, truncate(string(raw), 300))
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("飞书接口返回错误: code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Data, nil
}

// SendCard 向指定会话发送交互式消息卡片，返回 message_id。
func (c *Client) SendCard(ctx context.Context, chatID string, card any) (string, error) {
	if chatID == "" {
		chatID = c.DefaultChatID()
	}
	if chatID == "" {
		return "", errors.New("未配置接收群 chat_id")
	}
	content, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	data, err := c.call(ctx, http.MethodPost,
		"/open-apis/im/v1/messages?receive_id_type=chat_id",
		map[string]string{
			"receive_id": chatID,
			"msg_type":   "interactive",
			"content":    string(content),
		})
	if err != nil {
		return "", err
	}
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("解析发送结果失败: %w", err)
	}
	return out.MessageID, nil
}

// PatchCard 更新已发送卡片的样式与内容（点击按钮后替换成结果态）。
func (c *Client) PatchCard(ctx context.Context, messageID string, card any) error {
	if messageID == "" {
		return errors.New("消息 ID 为空")
	}
	content, err := json.Marshal(card)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, http.MethodPatch,
		"/open-apis/im/v1/messages/"+messageID,
		map[string]string{"content": string(content)})
	return err
}

// SendText 发送纯文本消息，用于连通性测试与错误提示。
func (c *Client) SendText(ctx context.Context, chatID, text string) (string, error) {
	if chatID == "" {
		chatID = c.DefaultChatID()
	}
	if chatID == "" {
		return "", errors.New("未配置接收群 chat_id")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", err
	}
	data, err := c.call(ctx, http.MethodPost,
		"/open-apis/im/v1/messages?receive_id_type=chat_id",
		map[string]string{
			"receive_id": chatID,
			"msg_type":   "text",
			"content":    string(content),
		})
	if err != nil {
		return "", err
	}
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.MessageID, nil
}

// FindChatIDByName 按群名称查询 chat_id，方便首次配置时确认。
func (c *Client) FindChatIDByName(ctx context.Context, name string) (string, error) {
	data, err := c.call(ctx, http.MethodGet,
		"/open-apis/im/v1/chats?page_size=100", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Items []struct {
			ChatID string `json:"chat_id"`
			Name   string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	for _, item := range out.Items {
		if item.Name == name {
			return item.ChatID, nil
		}
	}
	return "", fmt.Errorf("未找到名为 %q 的群，请确认机器人已加入该群", name)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
