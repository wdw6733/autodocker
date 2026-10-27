package store

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 首页那排数字必须只数**容器**。
//
// 曾经把全部镜像都算进去，结果总数 15、而「有更新 + 可选更新 + 已是最新」
// 只有 8 —— 中间那 7 条是没人用的悬空层和纯远端监控项。用户看到的是
// 「统计坏了」，其实是口径混了两类东西。这条测试就是钉住这个口径。
func TestCountImagesByStatusFiltersByOrigin(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	seed := []*model.Image{
		// 三个容器：一个待更新、一个已最新、一个检测失败
		{Reference: "nginx:1.25", ContainerID: "c1", ContainerName: "web", Origin: model.OriginContainer, Status: model.StatusUpdatePending},
		{Reference: "redis:7", ContainerID: "c2", ContainerName: "cache", Origin: model.OriginContainer, Status: model.StatusUpToDate},
		{Reference: "127.0.0.1:5000/myapp:latest", ContainerID: "c3", ContainerName: "app", Origin: model.OriginContainer, Status: model.StatusUnknown},
		// 两个本机镜像（没有容器在用）：一个悬空层、一个只是没用起来的新版本
		{Reference: "sha256:aaaa", Origin: model.OriginLocal, Dangling: true, Status: model.StatusUnknown},
		{Reference: "registry:3.1.2", Origin: model.OriginLocal, Status: model.StatusNewTag},
		// 一个纯远端监控项
		{Reference: "ghcr.io/foo/bar:latest", Origin: model.OriginWatch, Status: model.StatusUnknown},
	}
	for _, img := range seed {
		if _, err := s.UpsertImage(img); err != nil {
			t.Fatalf("写入 %s 失败: %v", img.Reference, err)
		}
	}

	all, err := s.CountImagesByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got := total(all); got != len(seed) {
		t.Errorf("不带 origin 时应统计全部 %d 条，得到 %d（%v）", len(seed), got, all)
	}

	got, err := s.CountImagesByStatus(model.OriginContainer)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		string(model.StatusUpdatePending): 1,
		string(model.StatusUpToDate):      1,
		string(model.StatusUnknown):       1,
	}
	if len(got) != len(want) {
		t.Fatalf("只数容器时应得到 %v，实际 %v", want, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("状态 %s 应为 %d，实际 %d", k, v, got[k])
		}
	}
	// 本机镜像与纯远端项不能被算进来 —— 这正是上一版数字对不上的原因
	if got[string(model.StatusNewTag)] != 0 {
		t.Errorf("没人用的镜像出新版不应计入首页「可选更新」，实际 %d", got[string(model.StatusNewTag)])
	}

	// 多个 origin 也要能一起统计
	both, err := s.CountImagesByStatus(model.OriginContainer, model.OriginWatch)
	if err != nil {
		t.Fatal(err)
	}
	if total(both) != 4 {
		t.Errorf("容器 + 纯远端应为 4 条，实际 %d（%v）", total(both), both)
	}
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// 容器的镜像引用换了写法，不能留下两行。
//
// 唯一键是 (reference, container_id)，容器还在的时候 DeleteImagesMissing
// 管不到旧行。实机出现过：同一个 ad-myapp 容器，一行 reference 是
// `127.0.0.1:5000/myapp:latest`，另一行是裸 digest，容器页上同一个容器出现两次。
func TestContainerUpsertRemovesStaleReferenceRow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const cid = "de6e3eefb61f"
	// 先按 tag 记一条
	if _, err := s.UpsertImage(&model.Image{
		Reference: "127.0.0.1:5000/myapp:latest", ContainerID: cid,
		ContainerName: "ad-myapp", Origin: model.OriginContainer, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	// 同一个容器，Docker 这次报的是裸 digest
	if _, err := s.UpsertImage(&model.Image{
		Reference: "sha256:6a9d11b53cada658", ContainerID: cid,
		ContainerName: "ad-myapp", Origin: model.OriginContainer, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListImages(model.OriginContainer)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("同一个容器只应留一行，实际 %d 行", len(rows))
	}
	if rows[0].Reference != "sha256:6a9d11b53cada658" {
		t.Errorf("应保留最新观测到的引用，实际 %q", rows[0].Reference)
	}

	// 另一个容器用同名 tag 是正常情况，不能被误删
	if _, err := s.UpsertImage(&model.Image{
		Reference: "127.0.0.1:5000/myapp:latest", ContainerID: "other-cid",
		ContainerName: "ad-myapp2", Origin: model.OriginContainer, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	// 纯远端项（container_id 为空）更不该被牵连
	if _, err := s.UpsertImage(&model.Image{
		Reference: "nginx:latest", Origin: model.OriginWatch, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListImages(model.OriginContainer)
	if len(rows) != 2 {
		t.Errorf("两个容器应各留一行，实际 %d 行", len(rows))
	}
	all, _ := s.ListImages()
	if len(all) != 3 {
		t.Errorf("加上纯远端项应为 3 行，实际 %d 行", len(all))
	}
}

// 「忽略本版本」必须真的落到监控项上，而且不能被下一轮扫描冲掉。
//
// 旧写法只改了通知卡片的 state，扫描端去重只看「有没有待处理通知」，
// 于是忽略完下一轮又新建一张 —— 卡片上那句「同一个版本不会再次提醒」是空话。
// UpsertImage 是扫描的落库入口，ON CONFLICT 分支若把 ignored_digest 一起刷成
// excluded 值，这个标记每轮都会被清空，抑制逻辑就形同虚设。这里钉住它。
func TestIgnoredDigestSurvivesUpsertAndClearsOnRestore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const ref, cid = "127.0.0.1:5000/myapp:latest", "c1"
	id, err := s.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "app",
		Origin: model.OriginContainer, Status: model.StatusUpdatePending,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 用户在飞书卡片上点了「忽略本版本」
	if err := s.SetImageIgnoredDigest(id, "sha256:new"); err != nil {
		t.Fatal(err)
	}

	// 下一轮扫描：同一容器再 Upsert 一次，标记必须原样留着
	if _, err := s.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "app",
		Origin: model.OriginContainer, Status: model.StatusUpdatePending,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	if got.IgnoredDigest != "sha256:new" {
		t.Fatalf("扫描落库后 ignored_digest 应保留为 sha256:new，实际 %q", got.IgnoredDigest)
	}

	// 恢复监控（点「恢复提醒」走的就是 SetImageIgnored(id,false)）要连标记一起清掉，
	// 否则用户点了恢复却还是收不到提醒。
	if err := s.SetImageIgnored(id, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.FindImage(ref, cid)
	if got.IgnoredDigest != "" {
		t.Errorf("恢复监控后 ignored_digest 应清空，实际 %q", got.IgnoredDigest)
	}
	if got.Ignored {
		t.Errorf("恢复监控后 ignored 应为 false")
	}
}
