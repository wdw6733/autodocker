package api

import (
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
)

// 「未使用镜像」这格的口径：按镜像 ID 去重、只要还有容器引用就不算。
// 数错了用户会照着去清理，结果是删了在用的镜像 —— 所以钉死在这里。
func TestCountUnusedImages(t *testing.T) {
	images := []dockerx.Image{
		{ID: "sha256:aaa", Tags: []string{"nginx:latest"}},         // 容器在用
		{ID: "sha256:bbb", Tags: []string{"redis:7", "redis:7.2"}}, // 没人用，两个 tag 算一个
		{ID: "sha256:ccc"}, // 悬空层，没人用，要算
		{ID: ""},           // 拿不到 ID 的读不懂，跳过
	}
	used := map[string]bool{"sha256:aaa": true}

	if got := countUnusedImages(images, used); got != 2 {
		t.Errorf("未使用镜像应为 2（redis + 悬空层），实际 %d", got)
	}

	// 同一 ID 重复出现（响应里有重复项时）不能数两遍
	dup := []dockerx.Image{{ID: "sha256:ddd"}, {ID: "sha256:ddd"}}
	if got := countUnusedImages(dup, nil); got != 1 {
		t.Errorf("同一镜像出现两次应只算一个，实际 %d", got)
	}

	// 全部在用 → 0
	if got := countUnusedImages(images[:1], used); got != 0 {
		t.Errorf("容器在用的镜像不该计入，实际 %d", got)
	}

	// 没有镜像 → 0
	if got := countUnusedImages(nil, used); got != 0 {
		t.Errorf("空列表应为 0，实际 %d", got)
	}
}

// GET 设置时凭据绝不能以明文回显：页面拿不到明文，
// 局域网里任何能打开这个端口的人就偷不走 Token/密码。
// 非敏感字段必须原样保留，不能让脱敏误伤表单里的其它输入。
func TestMaskSecrets(t *testing.T) {
	in := model.Settings{
		FeishuAppID:      "cli_x",
		FeishuAppSecret:  "feishu-secret",
		FeishuChatID:     "oc_x",
		TelegramBotToken: "123:abc",
		TelegramChatID:   "-100123",
		AdminUser:        "admin",
		AdminPassword:    "p@ss",
	}
	maskSecrets(&in)

	if in.FeishuAppSecret != secretMask {
		t.Errorf("FeishuAppSecret 应被掩码，得到 %q", in.FeishuAppSecret)
	}
	if in.TelegramBotToken != secretMask {
		t.Errorf("TelegramBotToken 应被掩码，得到 %q", in.TelegramBotToken)
	}
	if in.AdminPassword != secretMask {
		t.Errorf("AdminPassword 应被掩码，得到 %q", in.AdminPassword)
	}
	// 非敏感字段不能动
	if in.FeishuAppID != "cli_x" || in.FeishuChatID != "oc_x" ||
		in.TelegramChatID != "-100123" || in.AdminUser != "admin" {
		t.Errorf("脱敏不应改动非敏感字段: %+v", in)
	}
}

// PUT 收到掩码时表示「没改，保留旧值」；收到真实值或空串时尊重用户输入。
func TestKeepMaskedSecrets(t *testing.T) {
	prev := model.Settings{
		FeishuAppSecret:  "feishu-secret",
		TelegramBotToken: "123:abc",
		AdminPassword:    "p@ss",
	}
	in := model.Settings{
		FeishuAppSecret:  secretMask,  // 未改动 → 还原旧值
		TelegramBotToken: "new-token", // 用户改了 → 用新值
		AdminPassword:    "",          // 用户清空 → 尊重空值（关闭登录）
	}
	keepMaskedSecrets(&in, prev)

	if in.FeishuAppSecret != "feishu-secret" {
		t.Errorf("掩码应还原为旧值，得到 %q", in.FeishuAppSecret)
	}
	if in.TelegramBotToken != "new-token" {
		t.Errorf("用户新值不应被覆盖，得到 %q", in.TelegramBotToken)
	}
	if in.AdminPassword != "" {
		t.Errorf("空串是「显式清空」而不是掩码，应保持为空，得到 %q", in.AdminPassword)
	}
}
