package telegram

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// ActionHandler 处理一次按钮点击。
//
// 返回 toast 用于回应客户端（不回的话按钮会一直转圈），
// replace 非空时用它原地替换原消息，与飞书「换卡片」等价。
type ActionHandler interface {
	HandleCallback(ctx context.Context, v CallbackValue, chatID string, messageID int64) (toast string, replace *Message)
}

// ChatInfo 一个见过的会话，供设置页挑 chat_id 用。
type ChatInfo struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

// PollManager 管理长轮询的生命周期。
//
// 与飞书那边同一个理由：token 是装好之后在网页里填的，启动那一刻往往还没有；
// 填完不该逼用户重启。这里把「配置 → 轮询协程」做成可随时收敛的状态。
//
// 只要有 token 就开始轮询（chat_id 可以为空）：这样用户把 bot 拉进群、
// 随便说句话，设置页就能从「最近会话」里把 chat_id 读出来填上，
// 不必去翻 getUpdates 的 JSON。
type PollManager struct {
	baseCtx context.Context
	handler ActionHandler
	log     *slog.Logger

	mu      sync.Mutex
	cfg     Config
	cancel  context.CancelFunc
	running bool
	gen     int // 累计实际发起轮询的次数，仅用于观测

	seenMu sync.Mutex
	seen   map[int64]ChatInfo
	order  []int64

	errMu   sync.Mutex
	lastErr string
}

// NewPollManager 构造轮询管理器。
func NewPollManager(baseCtx context.Context, h ActionHandler, logger *slog.Logger) *PollManager {
	return &PollManager{baseCtx: baseCtx, handler: h, log: logger, seen: map[int64]ChatInfo{}}
}

// Ensure 让轮询收敛到给定凭据对应的状态。凭据没变时直接返回，
// 反复保存其它设置不会无谓地断开重连。
func (m *PollManager) Ensure(cfg Config) {
	cfg = normalize(cfg)

	m.mu.Lock()
	defer m.mu.Unlock()

	if cfg == m.cfg {
		return
	}
	m.stopLocked()
	m.cfg = cfg
	// 换配置就把旧报错清掉：那句话是拿上一套凭据/代理跑出来的，
	// 留着只会误导 —— 用户刚把代理填对、也保存了，结果页面还在报「直连超时」，
	// 看起来像保存没生效，实际是这条陈年错误还没被新的失败覆盖。
	m.setLastErr(nil)

	if cfg.Token == "" {
		m.log.Info("未配置 Telegram Bot Token，按钮回调轮询保持关闭")
		return
	}

	m.gen++
	ctx, cancel := context.WithCancel(m.baseCtx)
	m.cancel = cancel
	m.running = true

	client := New(cfg)
	go func() {
		defer func() {
			// 主动停止时不动 running：那多半是紧接着要换新配置，
			// 这里再置一次 false 会把后来者的状态误标成已停止。
			if ctx.Err() == nil {
				m.mu.Lock()
				m.running = false
				m.mu.Unlock()
			}
		}()
		m.run(ctx, client)
	}()
}

// Stop 关闭轮询（用于优雅退出）。
func (m *PollManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *PollManager) stopLocked() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.running = false
}

// Running 轮询协程是否已拉起。
func (m *PollManager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Chats 返回见过消息的会话，最近出现的排前面。
func (m *PollManager) Chats() []ChatInfo {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	out := make([]ChatInfo, 0, len(m.order))
	// order 是「出现的时间顺序」，倒着遍历就是最近优先
	for i := len(m.order) - 1; i >= 0; i-- {
		if c, ok := m.seen[m.order[i]]; ok {
			out = append(out, c)
		}
	}
	return out
}

// LastError 最近一次轮询失败的原因，空表示正常。
func (m *PollManager) LastError() string {
	m.errMu.Lock()
	defer m.errMu.Unlock()
	return m.lastErr
}

func (m *PollManager) setLastErr(err error) {
	m.errMu.Lock()
	defer m.errMu.Unlock()
	if err == nil {
		m.lastErr = ""
		return
	}
	m.lastErr = err.Error()
}

// rememberChat 记下见过的会话（同一会话只记一次，但会挪到最前面）。
func (m *PollManager) rememberChat(c Chat) {
	if c.ID == 0 {
		return
	}
	title := c.Title
	if title == "" {
		title = c.Type
	}
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	m.seen[c.ID] = ChatInfo{ID: c.ID, Type: c.Type, Title: title}
	// 挪到末尾（Chats 会反序返回，等价于最近优先）
	for i, id := range m.order {
		if id == c.ID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.order = append(m.order, c.ID)
}

// run 长轮询主循环。
func (m *PollManager) run(ctx context.Context, c *Client) {
	offset := int64(0)

	// 启动时先把积压的更新「消费掉」：offset=-1 让 Telegram 只返回最后一条，
	// 之后从它的 update_id+1 开始。少了这一步，进程重启会把停机期间
	// 别人点过的按钮重新执行一遍 —— 那些容器可能早就被手动处理过了。
	//
	// 但普通消息不能白丢：用户完全可能「先在群里说句话，再回来填 Token」，
	// 那句话说在轮询启动之前。这里如果不记，设置页的「读取会话」就永远是空的，
	// 而提示词还一个劲让人「再发一句话」，很容易被误判成网络或 Token 的问题。
	if ups, err := c.GetUpdates(ctx, -1, 0); err == nil {
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil {
				m.rememberChat(u.Message.Chat)
			}
		}
	} else if ctx.Err() == nil {
		m.log.Warn("Telegram 长轮询初始化失败（token 或网络有问题）", "err", err)
		m.setLastErr(err)
	}

	m.log.Info("Telegram 按钮回调长轮询已启动")
	for {
		if ctx.Err() != nil {
			return
		}
		ups, err := c.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.setLastErr(err)
			m.log.Warn("Telegram 长轮询失败，3 秒后重试", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		m.setLastErr(nil)
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			m.dispatch(ctx, c, u)
		}
	}
}

// dispatch 分发一条更新。
func (m *PollManager) dispatch(ctx context.Context, c *Client, u Update) {
	if u.Message != nil {
		// 普通消息只用来记录 chat_id，不回复 —— 用户在自己群里说话，
		// bot 抢着搭腔会很吵。
		m.rememberChat(u.Message.Chat)
		return
	}
	if u.CallbackQuery == nil {
		return
	}

	q := u.CallbackQuery
	var chatID int64
	var msgID int64
	if q.Message != nil {
		chatID, msgID = q.Message.Chat.ID, q.Message.MessageID
		m.rememberChat(q.Message.Chat)
	}

	v, err := DecodeCallback(q.Data)
	if err != nil {
		m.log.Warn("解析 Telegram 按钮数据失败", "data", q.Data, "err", err)
		_ = c.AnswerCallback(ctx, q.ID, "按钮数据无效")
		return
	}

	// 处理动作要脱离轮询上下文：这里只负责建任务，真正的重建在后台跑，
	// 而轮询的 ctx 会在换配置或退出时立刻取消。
	toast, replace := m.handler.HandleCallback(context.Background(), v, strconv.FormatInt(chatID, 10), msgID)

	if err := c.AnswerCallback(ctx, q.ID, toast); err != nil {
		m.log.Debug("回应 Telegram 按钮失败", "err", err)
	}
	if replace == nil || msgID == 0 {
		return
	}
	if err := c.EditMessageText(ctx, strconv.FormatInt(chatID, 10), msgID, *replace); err != nil {
		m.log.Warn("改写 Telegram 消息失败", "err", err)
	}
}
