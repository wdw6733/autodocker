// Package notify 承担两件事：
//  1. 把扫描发现的更新推送成可交互消息（飞书卡片 / Telegram 带按钮的消息）；
//  2. 接收按钮点击，落更新任务并异步执行，再把结果写回同一条消息。
//
// 同时配了多个渠道时，一条提醒会**分别**发到每个就绪的渠道 ——
// 用户把两个渠道都填好了，就是两边都想收到，只发其中一个会让人以为坏掉了。
// 每条渠道的消息各记一份坐标（Notification.Deliveries），
// 更新完成后才改得动全部卡片，不会留下一个永远停在「待更新」的。
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/notify/feishu"
	"github.com/haihaipypy/autodocker/internal/notify/telegram"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
)

// 与渠道无关的动作标识。
//
// 两个渠道包各自也定义了同名常量（按钮的 callback_data / value 里存的就是这几个字符串），
// service_test.go 里有一条断言把三处钉死，改了一边会立刻红。
const (
	ActionUpdate = "update"
	ActionIgnore = "ignore"
	ActionSnooze = "snooze"
	ActionTest   = "test"
)

// TargetRef 计算本次更新真正要拉取的镜像引用。
//
// 两种通知的语义并不一样，这是最容易搞错的一处：
//   - 「浮动 tag 内容变更」：容器跑 nginx:latest，远端内容变了 → 还是拉 nginx:latest；
//   - 「仓库出现更高版本 tag」：容器跑 nginx:1.25，仓库里有 1.27 →
//     ImageRef 记录的仍是旧的 1.25，真正要拉的是 1.27。
//
// 因此不能在 new_tag 场景直接使用 ImageRef，否则会变成「把旧版本再拉一遍」的空操作。
func TargetRef(n *model.Notification) string {
	ref, err := registry.ParseReference(n.ImageRef)
	if err != nil {
		return n.ImageRef
	}
	// LatestTag 只在发现更高版本 tag 时才会被写入，且必然与当前 tag 不同
	if n.LatestTag == "" || n.LatestTag == ref.Tag {
		return n.ImageRef
	}
	return ref.WithTag(n.LatestTag)
}

// Service 通知与交互服务。
type Service struct {
	fs  *feishu.Client
	lc  *feishu.LongConnManager
	tg  *telegram.Client
	tp  *telegram.PollManager
	st  *store.Store
	up  *updater.Updater
	log *slog.Logger

	baseCtx context.Context

	// 设置里选的渠道（可能为空 = 未指定）。真正发消息时还要看这个渠道配没配。
	chMu    sync.RWMutex
	channel model.Channel

	// 按钮回调的观测记录。发消息与收按钮回调是两条彼此独立的链路：
	// 消息能收到并不代表按钮点得动。把最近一次回调记下来，
	// 设置页就能给出确定结论，用户不必去翻容器日志猜。
	cbMu     sync.Mutex
	cbAt     time.Time
	cbAction string
	cbChan   model.Channel
	cbCount  int
}

// New 构造通知服务。baseCtx 用于派生异步更新任务的上下文。
func New(ctx context.Context, fs *feishu.Client, st *store.Store, up *updater.Updater, logger *slog.Logger) *Service {
	svc := &Service{
		fs:      fs,
		tg:      telegram.New(telegram.Config{}),
		st:      st,
		up:      up,
		log:     logger,
		baseCtx: ctx,
	}
	// Service 自己就是按钮回调的处理方，直接把 this 交给两个渠道的连接管理器
	svc.lc = feishu.NewLongConnManager(ctx, svc, logger)
	svc.tp = telegram.NewPollManager(ctx, svc, logger)
	return svc
}

// Feishu 暴露底层飞书客户端，供 API 层做配置与测试。
func (s *Service) Feishu() *feishu.Client { return s.fs }

// Telegram 暴露底层 Telegram 客户端。
func (s *Service) Telegram() *telegram.Client { return s.tg }

// ApplyChannel 记录用户选的渠道。留空表示「谁配好了就用谁」，飞书优先。
func (s *Service) ApplyChannel(ch string) {
	s.chMu.Lock()
	defer s.chMu.Unlock()
	s.channel = model.Channel(ch)
}

// EffectiveChannel 设置页当前选中的渠道，仅用于展示与默认值。
//
// **不再决定消息发给谁** —— 1.2.10 起一条提醒会广播到所有就绪渠道（见 readyChannels）。
// 显式选了某个渠道但它没配好时返回空，而不是偷偷换成另一个。
func (s *Service) EffectiveChannel() model.Channel {
	s.chMu.RLock()
	ch := s.channel
	s.chMu.RUnlock()

	switch ch {
	case model.ChannelFeishu:
		if s.fs.Configured() {
			return model.ChannelFeishu
		}
		return ""
	case model.ChannelTelegram:
		if s.tg.Configured() {
			return model.ChannelTelegram
		}
		return ""
	}
	// 未指定：保持历史行为，飞书优先
	if s.fs.Configured() {
		return model.ChannelFeishu
	}
	if s.tg.Configured() {
		return model.ChannelTelegram
	}
	return ""
}

// NotifyReady 是否至少有一个渠道能把消息真正发出去。
//
// 改成多渠道广播之后，一条提醒会发到**所有**就绪渠道，
// 所以这里问的是「有没有一个能发」，而不是「选中的那个能不能发」——
// 后者在「选了 Telegram 但只配好了飞书」时会给假的否。
func (s *Service) NotifyReady() bool {
	return len(s.readyChannels()) > 0
}

// ApplyFeishu 应用一份飞书配置并确保长连接相应收敛。
//
// 网页设置保存后由 API 层调用；凭据留空即视为关闭飞书通知，
// 长连接也会一并停掉，避免拿着无效凭据反复重连刷日志。
func (s *Service) ApplyFeishu(appID, appSecret, chatID string) {
	s.fs.SetCredentials(appID, appSecret)
	s.fs.SetChatID(chatID)
	s.lc.Ensure(appID, appSecret)
}

// ApplyTelegram 应用一份 Telegram 配置并让长轮询收敛到对应状态。
//
// 只有 token 就会开始轮询（chat_id 可以为空）：用户把 bot 拉进群说句话，
// 设置页就能从「最近会话」里把 chat_id 挑出来，不必去手工翻 getUpdates 的返回。
func (s *Service) ApplyTelegram(cfg telegram.Config) {
	s.tg.SetOptions(cfg)
	s.tp.Ensure(cfg)
}

// LongConnRunning 返回飞书卡片回调长连接是否已就绪。
func (s *Service) LongConnRunning() bool { return s.lc.Running() }

// TelegramPolling 返回 Telegram 按钮回调的长轮询是否在跑。
func (s *Service) TelegramPolling() bool { return s.tp.Running() }

// ChannelStatus 单个通知渠道的可用状态，供顶栏胶囊展示。
//
// Configured 说的是「写得出去」：凭据与 chat_id 都填了；
// Online 说的是「链路真的通」—— 不能只看轮询/长连接有没有拉起，
// 那只是 goroutine 活着，token 写错、代理不通时它照样是 true，
// 界面就会长成一副岁月静好的样子。
//
// 两个渠道可能同时配好，所以这里返回列表而不是单个值：
// 顶栏只说「已连接」，具体是哪几个挂 title 上。
type ChannelStatus struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Online     bool   `json:"online"`
	Error      string `json:"error,omitempty"`
}

// ChannelStatuses 列出所有填过凭据的渠道及其链路状态。
func (s *Service) ChannelStatuses() []ChannelStatus {
	out := make([]ChannelStatus, 0, 2)

	if s.fs.Configured() {
		// 飞书的 SDK 自己带重连，没有可读的错误状态，只能看长连接在不在。
		ready := s.fs.DefaultChatID() != ""
		out = append(out, ChannelStatus{
			Name:       "飞书",
			Configured: ready,
			Online:     ready && s.lc.Running(),
		})
	}
	if s.tg.Configured() {
		errText := s.tp.LastError()
		ready := s.tg.DefaultChatID() != ""
		out = append(out, ChannelStatus{
			Name:       "Telegram",
			Configured: ready,
			Online:     ready && s.tp.Running() && errText == "",
			Error:      errText,
		})
	}
	return out
}

// TelegramChats 最近在这个 bot 上出现过消息的会话。
func (s *Service) TelegramChats() []telegram.ChatInfo { return s.tp.Chats() }

// TelegramLastError 最近一次长轮询失败的原因。
func (s *Service) TelegramLastError() string { return s.tp.LastError() }

// Close 关闭两条按钮回调链路，用于进程退出前的收尾。
func (s *Service) Close() {
	s.lc.Stop()
	s.tp.Stop()
}

// recordCallback 记下一次按钮回调，返回其时间戳。
func (s *Service) recordCallback(ch model.Channel, action string) time.Time {
	now := time.Now()
	s.cbMu.Lock()
	s.cbAt, s.cbAction, s.cbChan = now, action, ch
	s.cbCount++
	s.cbMu.Unlock()
	return now
}

// CallbackState 返回最近一次按钮回调的观测结果。
// at 为零值表示服务启动以来还没收到过任何回调 —— 这正是设置页要显示的信息。
func (s *Service) CallbackState() (at time.Time, action string, ch model.Channel, count int) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	return s.cbAt, s.cbAction, s.cbChan, s.cbCount
}

// readyChannels 返回「凭据填了、接收会话也有」的渠道，按飞书优先。
//
// 判 chat_id 而不是只看 Configured：应用凭据填完长连接就绿了，
// 看着像配好了，实际一条消息也发不到任何地方。
func (s *Service) readyChannels() []model.Channel {
	out := make([]model.Channel, 0, 2)
	if s.fs.Configured() && s.fs.DefaultChatID() != "" {
		out = append(out, model.ChannelFeishu)
	}
	if s.tg.Configured() && s.tg.DefaultChatID() != "" {
		out = append(out, model.ChannelTelegram)
	}
	return out
}

// Notify 实现 scanner.Notifier：向所有就绪渠道发送更新提醒，返回主投递的消息 ID。
//
// 一个渠道失败不影响另一个：两边都配了却只收到一条，也比一条都没收到强。
// 只有**全部**失败才返回错误，让扫描器把失败原因记进日志。
func (s *Service) Notify(ctx context.Context, n *model.Notification) (string, error) {
	chans := s.readyChannels()
	if len(chans) == 0 {
		n.Channel = ""
		s.log.Debug("未配置通知渠道，提醒仅落库", "ref", n.ImageRef)
		return "", nil
	}

	var (
		deliveries []model.Delivery
		failures   []string
	)

	for _, ch := range chans {
		switch ch {
		case model.ChannelTelegram:
			chatID := s.tg.DefaultChatID()
			msgID, err := s.tg.SendMessage(ctx, chatID, telegram.BuildUpdateMessage(n))
			if err != nil {
				failures = append(failures, "Telegram："+err.Error())
				s.log.Warn("Telegram 推送失败", "ref", n.ImageRef, "err", err)
				continue
			}
			deliveries = append(deliveries, model.Delivery{
				Channel:   model.ChannelTelegram,
				ChatRef:   chatID,
				MessageID: strconv.FormatInt(msgID, 10),
			})
			s.log.Info("已推送更新提醒", "channel", "telegram", "ref", n.ImageRef, "message_id", msgID)

		case model.ChannelFeishu:
			chatID := s.fs.DefaultChatID()
			msgID, err := s.fs.SendCard(ctx, "", feishu.BuildUpdateCard(n))
			if err != nil {
				failures = append(failures, "飞书："+err.Error())
				s.log.Warn("飞书推送失败", "ref", n.ImageRef, "err", err)
				continue
			}
			deliveries = append(deliveries, model.Delivery{
				Channel:   model.ChannelFeishu,
				ChatRef:   chatID,
				MessageID: msgID,
			})
			s.log.Info("已推送更新提醒", "channel", "feishu", "ref", n.ImageRef, "message_id", msgID)
		}
	}

	if len(deliveries) == 0 {
		return "", errors.New("所有通知渠道都发送失败：" + strings.Join(failures, "；"))
	}
	if len(failures) > 0 {
		s.log.Warn("部分渠道推送失败", "ref", n.ImageRef, "failures", strings.Join(failures, "；"))
	}

	// 主投递 = 第一条发成功的，兼容只认单渠道的老代码与前端展示。
	n.Deliveries = deliveries
	n.Channel = deliveries[0].Channel
	n.ChatRef = deliveries[0].ChatRef
	n.MessageID = deliveries[0].MessageID
	return deliveries[0].MessageID, nil
}

// handleCardAction 实现 feishu.ActionHandler：处理飞书卡片按钮点击。
//
// 必须在 3 秒内返回，因此这里只创建任务并立刻返回「进行中」的卡片，
// 真正的重建放到后台 goroutine 里跑。
func (s *Service) HandleCardAction(ctx context.Context, v feishu.CardValue) (string, map[string]any) {
	at := s.recordCallback(model.ChannelFeishu, v.Action)

	// 测试卡片上的按钮不关联任何通知，必须在查库**之前**处理，
	// 否则会掉进下面「通知已失效」那条分支 —— 表现为点了按钮提示
	// 「通知已失效」，让人以为是自己配错了。
	if v.Action == ActionTest {
		s.log.Info("收到飞书测试卡片回调，按钮链路正常")
		return "回调已收到", feishu.BuildTestDoneCard(at)
	}

	n, err := s.st.GetNotification(v.NID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "通知已失效", nil
		}
		s.log.Warn("读取通知失败", "nid", v.NID, "err", err)
		return "服务异常", nil
	}

	out := s.applyAction(n, v.Action, model.ChannelFeishu)
	if out.State == "" {
		return out.Toast, nil
	}
	return out.Toast, feishu.BuildStateCard(n, out.State, out.Detail)
}

// HandleCallback 实现 telegram.ActionHandler：处理按钮点击。
//
// 返回值：toast 用于回应客户端（不回按钮会一直转圈），replace 用于原地改消息。
func (s *Service) HandleCallback(ctx context.Context, v telegram.CallbackValue, chatID string, messageID int64) (string, *telegram.Message) {
	at := s.recordCallback(model.ChannelTelegram, v.Action)

	if v.Action == ActionTest {
		s.log.Info("收到 Telegram 测试消息回调，按钮链路正常")
		m := telegram.BuildTestDoneMessage(at)
		return "回调已收到", &m
	}

	n, err := s.st.GetNotification(v.NID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "通知已失效", nil
		}
		s.log.Warn("读取通知失败", "nid", v.NID, "err", err)
		return "服务异常", nil
	}

	// 按钮所在会话才是权威坐标：通知是按默认会话发的，
	// 但同一条消息被转发到别的群后，点按钮拿到的是转发后那个会话。
	//
	// 只改 Telegram 这一份投递记录 —— 主投递很可能是飞书的 chip，
	// 顺手覆盖它会把飞书的会话 ID 写成 Telegram 的 chat_id。
	if chatID != "" {
		s.fixTelegramChatRef(n, chatID)
	}

	out := s.applyAction(n, v.Action, model.ChannelTelegram)
	if out.State == "" {
		return out.Toast, nil
	}
	m := telegram.BuildStateMessage(n, out.State, out.Detail)
	return out.Toast, &m
}

// fixTelegramChatRef 用「点按钮时所在的会话」纠正 Telegram 那份投递坐标。
func (s *Service) fixTelegramChatRef(n *model.Notification, chatID string) {
	if len(n.Deliveries) == 0 {
		// 老库：只有主投递一份，且它必然是 Telegram 的（1.2.4 之前没有多渠道路径）。
		if n.ChatRef == chatID {
			return
		}
		if err := s.st.SetNotificationChatRef(n.ID, chatID); err != nil {
			s.log.Warn("记录 Telegram 会话失败", "nid", n.ID, "err", err)
		}
		n.ChatRef = chatID
		return
	}

	changed := false
	for i := range n.Deliveries {
		if n.Deliveries[i].Channel == model.ChannelTelegram && n.Deliveries[i].ChatRef != chatID {
			n.Deliveries[i].ChatRef = chatID
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := s.st.UpdateNotificationDeliveries(n.ID, n.Deliveries); err != nil {
		s.log.Warn("记录 Telegram 会话失败", "nid", n.ID, "err", err)
	}
	if n.Channel == model.ChannelTelegram {
		n.ChatRef = chatID
	}
}

// actionOutcome 一次按钮点击的处理结果（与具体渠道无关）。
type actionOutcome struct {
	Toast  string
	State  model.CardState // 空表示不需要改写原消息
	Detail string
}

// applyAction 是所有渠道（飞书 / Telegram / 将来的其它）共用的落地逻辑。
//
// 带上 from（哪个渠道点的）：主投递恒为飞书，用它来记「谁触发的更新」
// 会让 Telegram 里点的按钮在历史里显示成飞书点的。
func (s *Service) applyAction(n *model.Notification, action string, from model.Channel) actionOutcome {
	switch action {
	case ActionIgnore:
		if err := s.st.SetNotificationCardState(n.ID, model.CardIgnored); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
		_ = s.st.MarkNotificationRead(n.ID, true)
		// 光把这条通知标成「已忽略」是拦不住下一条消息的：扫描只看远端摘要变没变，
		// 下一轮照样判成「有更新」并新建一条通知。必须把「用户已放行这一版」
		// 记到监控项上，消息里那句「同一个版本不会再次提醒」才是真的。
		s.rememberIgnoredDigest(n)
		return actionOutcome{
			Toast:  "已忽略该版本",
			State:  model.CardIgnored,
			Detail: "已忽略本版本，同一个版本不会再次提醒。\n后续出现更高的版本时仍会通知。",
		}

	case ActionSnooze:
		if err := s.st.SetNotificationCardState(n.ID, model.CardSnoozed); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
		return actionOutcome{
			Toast:  "已设为稍后",
			State:  model.CardSnoozed,
			Detail: "已记录。本轮巡检不会重复提醒，下次巡检如有更新会重新推送。",
		}

	case ActionUpdate:
		if n.CardState == model.CardUpdating {
			return actionOutcome{Toast: "更新已在进行中"}
		}
		taskID, err := s.TriggerUpdate(n, string(from))
		if err != nil {
			return actionOutcome{Toast: "无法更新", State: model.CardFailed, Detail: err.Error()}
		}
		return actionOutcome{
			Toast: "已开始更新",
			State: model.CardUpdating,
			Detail: fmt.Sprintf(
				"已受理，正在拉取镜像并重建容器（任务 #%d）。\n完成后本条消息会自动更新结果，期间无需其他操作。", taskID),
		}

	default:
		return actionOutcome{Toast: "未知操作"}
	}
}

// rememberIgnoredDigest 把「用户放行的这次远端内容」记到监控项上。
//
// 记的是库里**当前**的远端摘要，而不是通知里那份快照：用户点按钮时想放行的是
// 「现在仓库里这一版」，以最新观测为准更稳妥（期间要是又推了一版，下一轮该提醒还得提醒）。
// 记录失败不影响回调返回 —— 按钮已经点了，消息状态也改了，这里只是尽力而为，
// 真失败了顶多下次再被提醒一遍。
func (s *Service) rememberIgnoredDigest(n *model.Notification) {
	if n.ContainerID == "" {
		return
	}
	img, err := s.st.FindImage(n.ImageRef, n.ContainerID)
	if err != nil {
		s.log.Warn("读取监控项失败，无法记录已忽略版本", "ref", n.ImageRef, "err", err)
		return
	}
	if img.RemoteDigest == "" {
		return
	}
	if err := s.st.SetImageIgnoredDigest(img.ID, img.RemoteDigest); err != nil {
		s.log.Warn("记录已忽略版本失败", "ref", n.ImageRef, "err", err)
	}
}

// TriggerUpdate 创建并异步执行一次更新任务，返回任务 ID。
// 三个入口（飞书按钮 / Telegram 按钮 / Web 页面）共用这一处，行为才保证一致。
func (s *Service) TriggerUpdate(n *model.Notification, triggeredBy string) (int64, error) {
	if n.ContainerID == "" {
		return 0, errors.New("该监控项没有关联的本地容器，无法执行更新")
	}
	if s.up.Busy() {
		return 0, errors.New("已有更新任务正在执行，请稍后重试")
	}
	if busy, err := s.st.HasRunningTask(n.ContainerID); err == nil && busy {
		return 0, errors.New("该容器已有进行中的更新任务")
	}

	task := &model.Task{
		NotificationID: n.ID,
		ImageRef:       TargetRef(n),
		ContainerID:    n.ContainerID,
		ContainerName:  n.ContainerName,
		TriggeredBy:    triggeredBy,
	}
	taskID, err := s.st.CreateTask(task)
	if err != nil {
		s.log.Error("创建更新任务失败", "err", err)
		return 0, fmt.Errorf("创建更新任务失败: %w", err)
	}
	if n.ID > 0 {
		if err := s.st.SetNotificationCardState(n.ID, model.CardUpdating); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
	}

	go s.runUpdate(n, taskID, triggeredBy)
	return taskID, nil
}

// runUpdate 在后台执行更新，并把最终结果写回原消息。
func (s *Service) runUpdate(n *model.Notification, taskID int64, triggeredBy string) {
	ctx, cancel := context.WithTimeout(s.baseCtx, 45*time.Minute)
	defer cancel()

	target := TargetRef(n)
	s.log.Info("开始执行更新", "task", taskID, "container", n.ContainerName,
		"from", n.ImageRef, "target", target, "by", triggeredBy)

	err := s.up.Run(ctx, updater.Request{
		TaskID:        taskID,
		ImageRef:      target,
		ContainerID:   n.ContainerID,
		ContainerName: n.ContainerName,
		TriggeredBy:   triggeredBy,
	})

	state := model.CardDone
	detail := "容器已在新镜像上重建完成，原有挂载、端口与网络配置保持不变。"
	if err != nil {
		state = model.CardFailed
		detail = "更新失败：" + err.Error()
		if t, terr := s.st.GetTask(taskID); terr == nil && t.Status == model.TaskRolledBack {
			state = model.CardRollback
			detail = "新容器未通过健康检查，已自动回滚到原版本（原容器已恢复运行）。\n原因：" + err.Error()
		}
	}

	if n.ID > 0 {
		if serr := s.st.SetNotificationCardState(n.ID, state); serr != nil {
			s.log.Warn("写回通知状态失败", "err", serr)
		}
		_ = s.st.MarkNotificationRead(n.ID, true)
	}

	// 原地替换原消息，形成可追溯的操作记录。
	// 两个渠道各发过一份，就必须两条都改 —— 漏掉的那条会永远停在「待更新」，
	// 用户点进去还以为是没生效。
	targets := s.deliveryTargets(n)
	if len(targets) == 0 {
		return
	}
	pushCtx, cancelPush := context.WithTimeout(s.baseCtx, 30*time.Second)
	defer cancelPush()
	for _, d := range targets {
		s.editDelivery(pushCtx, n, d, state, detail)
	}
}

// deliveryTargets 取这条通知的全部投递坐标。
//
// 老库（1.2.4 之前建的记录）没有 deliveries，退回主投递的那一份。
func (s *Service) deliveryTargets(n *model.Notification) []model.Delivery {
	if len(n.Deliveries) > 0 {
		return n.Deliveries
	}
	if n.MessageID == "" {
		return nil
	}
	return []model.Delivery{{Channel: n.Channel, ChatRef: n.ChatRef, MessageID: n.MessageID}}
}

// editDelivery 把某个渠道上那张卡片/消息改成最终状态。
func (s *Service) editDelivery(ctx context.Context, n *model.Notification, d model.Delivery, state model.CardState, detail string) {
	switch d.Channel {
	case model.ChannelTelegram:
		if !s.tg.Configured() {
			return
		}
		chatID := d.ChatRef
		if chatID == "" {
			chatID = s.tg.DefaultChatID()
		}
		msgID, err := strconv.ParseInt(d.MessageID, 10, 64)
		if err != nil {
			s.log.Warn("Telegram 消息 ID 非法", "message_id", d.MessageID, "err", err)
			return
		}
		if err := s.tg.EditMessageText(ctx, chatID, msgID, telegram.BuildStateMessage(n, state, detail)); err != nil {
			s.log.Warn("更新 Telegram 消息失败", "message_id", d.MessageID, "err", err)
		}
	default:
		if !s.fs.Configured() {
			return
		}
		if err := s.fs.PatchCard(ctx, d.MessageID, feishu.BuildStateCard(n, state, detail)); err != nil {
			s.log.Warn("更新飞书卡片失败", "message_id", d.MessageID, "err", err)
		}
	}
}

// SendTestCard 发送一张飞书测试卡片，用于部署后验证配置。
func (s *Service) SendTestCard(ctx context.Context) error {
	if !s.fs.Configured() {
		return errors.New("飞书应用未配置，请先填写 App ID 与 App Secret")
	}
	chatID := s.fs.DefaultChatID()
	if chatID == "" {
		return errors.New("尚未配置接收群 chat_id")
	}
	_, err := s.fs.SendCard(ctx, chatID, feishu.BuildTestCard(chatID))
	return err
}

// SendTestTelegram 发送一条带按钮的 Telegram 测试消息。
//
// 与飞书同一个道理：能收到消息不代表按钮点得动，所以测试消息必须带按钮，
// 点一下、原地变成回执，才算把「发消息 + 收回调」整条链路验完。
func (s *Service) SendTestTelegram(ctx context.Context) error {
	if !s.tg.Configured() {
		return errors.New("Telegram Bot Token 未配置")
	}
	// 先确认 token 有效、网络通：token 错了 sendMessage 的报错是「unauthorized」，
	// 看着像聊天 ID 的问题，先跑一遍 getMe 能把结论说清楚。
	// 顺序很重要 —— 如果先检查 chat_id，网络不通的用户会先看到「没有会话」，
	// 于是回去折腾会话 ID，而真正的问题在出网上。
	if name, err := s.tg.GetMe(ctx); err != nil {
		return fmt.Errorf("Bot Token 校验失败: %w", err)
	} else if name != "" {
		s.log.Info("Telegram bot 校验通过", "bot", name)
	}
	chatID := s.tg.DefaultChatID()
	if chatID == "" {
		return errors.New("尚未配置接收会话 chat_id（把 bot 拉进群 @它 说句话，点「读取会话」）")
	}
	_, err := s.tg.SendMessage(ctx, chatID, telegram.BuildTestMessage(chatID))
	return err
}
