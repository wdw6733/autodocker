package store

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 批次要能完整走一遍「创建 → 读取 → 回写投递 → 更新进度 → 再读取」，
// Items 与 Deliveries 都以 JSON 落库，任何一步把字段丢了都会让
// 「汇总卡更新不了」或「点了按钮不知道更新哪些容器」。
func TestBatchRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	items := []model.BatchItem{
		{
			NotificationID: 11,
			ImageRef:       "nginx:1.25",
			ContainerID:    "c1",
			ContainerName:  "web",
			Kind:           model.NotifNewTag,
			LocalTag:       "1.25",
			LatestTag:      "1.27",
			LocalSummary:   "abc123",
			RemoteSummary:  "def456",
		},
		{
			NotificationID: 12,
			ImageRef:       "redis:latest",
			ContainerID:    "c2",
			ContainerName:  "cache",
			Kind:           model.NotifNewVersion,
			LocalTag:       "latest",
		},
	}
	batch := &model.UpdateBatch{
		Status:      model.BatchPending,
		TriggeredBy: "scan",
		Total:       len(items),
		Items:       items,
	}
	id, err := s.CreateBatch(batch)
	if err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if id == 0 || batch.ID != id {
		t.Fatalf("批次 ID 应回填，得到 id=%d batch.ID=%d", id, batch.ID)
	}

	got, err := s.GetBatch(id)
	if err != nil {
		t.Fatalf("读取批次失败: %v", err)
	}
	if got.Status != model.BatchPending || got.TriggeredBy != "scan" || got.Total != 2 {
		t.Fatalf("批次基础字段不符: %+v", got)
	}
	if len(got.Items) != 2 || got.Items[0].ContainerID != "c1" || got.Items[0].LatestTag != "1.27" {
		t.Fatalf("批次条目不符: %+v", got.Items)
	}

	deliveries := []model.Delivery{
		{Channel: model.ChannelTelegram, ChatRef: "-100123", MessageID: "456"},
	}
	if err := s.SetBatchDeliveries(id, deliveries); err != nil {
		t.Fatalf("写入投递坐标失败: %v", err)
	}
	if err := s.UpdateBatchProgress(id, model.BatchDone, 2, 0, 0, 0); err != nil {
		t.Fatalf("更新批次进度失败: %v", err)
	}

	done, err := s.GetBatch(id)
	if err != nil {
		t.Fatalf("再次读取批次失败: %v", err)
	}
	if done.Status != model.BatchDone || done.Succeeded != 2 || done.Failed != 0 {
		t.Fatalf("批次进度不符: %+v", done)
	}
	if len(done.Deliveries) != 1 || done.Deliveries[0].MessageID != "456" {
		t.Fatalf("投递坐标不符: %+v", done.Deliveries)
	}
	if done.FinishedAt == nil {
		t.Fatal("批次完成后 FinishedAt 不应为空")
	}
}

// 默认状态：未显式指定时批次应处于 pending，而不是空串。
func TestCreateBatchDefaultsPending(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	id, err := s.CreateBatch(&model.UpdateBatch{TriggeredBy: "web", Total: 1})
	if err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	got, err := s.GetBatch(id)
	if err != nil {
		t.Fatalf("读取批次失败: %v", err)
	}
	if got.Status != model.BatchPending {
		t.Fatalf("未指定状态时应为 pending，得到 %q", got.Status)
	}
}

// 批次也要能落到任务上：CreateTask 写入 BatchID，读取时原样返回。
func TestTaskCarriesBatchID(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	batchID, err := s.CreateBatch(&model.UpdateBatch{Status: model.BatchRunning, TriggeredBy: "web", Total: 1})
	if err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	taskID, err := s.CreateTask(&model.Task{
		BatchID:       batchID,
		ImageRef:      "nginx:1.27",
		ContainerID:   "c1",
		ContainerName: "web",
		TriggeredBy:   "web",
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if task.BatchID != batchID {
		t.Fatalf("任务应记录 BatchID=%d，得到 %d", batchID, task.BatchID)
	}
}
