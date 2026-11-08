package api

import (
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
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
