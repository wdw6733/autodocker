package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

// CreateBatch 写入一个批量更新批次，返回批次 ID。
func (s *Store) CreateBatch(b *model.UpdateBatch) (int64, error) {
	if b.Status == "" {
		b.Status = model.BatchPending
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	items, err := json.Marshal(b.Items)
	if err != nil {
		items = []byte("[]")
	}
	deliveries, derr := json.Marshal(b.Deliveries)
	if derr != nil || len(b.Deliveries) == 0 {
		deliveries = []byte("[]")
	}
	var id int64
	err = s.db.QueryRow(`
		INSERT INTO update_batches(status, triggered_by, total, items, deliveries, created_at)
		VALUES(?,?,?,?,?,?)
		RETURNING id`,
		string(b.Status), b.TriggeredBy, b.Total, string(items), string(deliveries),
		timeToTs(b.CreatedAt),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	b.ID = id
	return id, nil
}

const batchCols = `id, status, triggered_by, total, succeeded, failed, rolled_back, skipped,
	items, deliveries, created_at, finished_at`

func scanBatch(sc rowScanner) (model.UpdateBatch, error) {
	var (
		b             model.UpdateBatch
		status        string
		itemsJSON     string
		deliveriesJSON string
		createdAt     int64
		finishedAt    int64
	)
	err := sc.Scan(
		&b.ID, &status, &b.TriggeredBy, &b.Total, &b.Succeeded, &b.Failed, &b.RolledBack, &b.Skipped,
		&itemsJSON, &deliveriesJSON, &createdAt, &finishedAt,
	)
	if err != nil {
		return b, err
	}
	b.Status = model.BatchStatus(status)
	b.CreatedAt = tsToTime(createdAt)
	b.FinishedAt = tsToTimePtr(finishedAt)
	if itemsJSON != "" {
		_ = json.Unmarshal([]byte(itemsJSON), &b.Items)
	}
	if deliveriesJSON != "" {
		_ = json.Unmarshal([]byte(deliveriesJSON), &b.Deliveries)
	}
	return b, nil
}

// GetBatch 按主键读取批次。
func (s *Store) GetBatch(id int64) (*model.UpdateBatch, error) {
	b, err := scanBatch(s.db.QueryRow(`SELECT `+batchCols+` FROM update_batches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// SetBatchDeliveries 写入批次汇总卡/消息的投递坐标。
func (s *Store) SetBatchDeliveries(id int64, list []model.Delivery) error {
	if len(list) == 0 {
		return nil
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE update_batches SET deliveries = ? WHERE id = ?`, string(raw), id)
	return err
}

// UpdateBatchProgress 更新批次计数与状态。
func (s *Store) UpdateBatchProgress(id int64, status model.BatchStatus, succeeded, failed, rolledBack, skipped int) error {
	finishedAt := int64(0)
	if status == model.BatchDone {
		finishedAt = timeToTs(time.Now())
	}
	_, err := s.db.Exec(`
		UPDATE update_batches
		SET status = ?, succeeded = ?, failed = ?, rolled_back = ?, skipped = ?, finished_at = ?
		WHERE id = ?`,
		string(status), succeeded, failed, rolledBack, skipped, finishedAt, id,
	)
	return err
}
