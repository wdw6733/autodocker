package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

const scanCols = `id, started_at, finished_at, total, with_update, notified, forced, error`

func scanScan(sc rowScanner) (model.Scan, error) {
	var (
		s          model.Scan
		startedAt  int64
		finishedAt int64
		forced     int
	)
	err := sc.Scan(&s.ID, &startedAt, &finishedAt, &s.Total, &s.WithUpdate, &s.Notified, &forced, &s.Error)
	if err != nil {
		return s, err
	}
	s.StartedAt = tsToTime(startedAt)
	s.FinishedAt = tsToTime(finishedAt)
	s.Forced = forced != 0
	return s, nil
}

// CreateScan 创建一次扫描记录。
func (s *Store) CreateScan(forced bool) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO scans(started_at, forced) VALUES(?, ?) RETURNING id`,
		timeToTs(time.Now()), b2i(forced),
	).Scan(&id)
	return id, err
}

// FinishScan 结束扫描记录。
func (s *Store) FinishScan(id int64, total, withUpdate, notified int, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE scans SET finished_at = ?, total = ?, with_update = ?, notified = ?, error = ? WHERE id = ?`,
		timeToTs(time.Now()), total, withUpdate, notified, errMsg, id,
	)
	return err
}

// ListScans 列出扫描历史。
func (s *Store) ListScans(limit int) ([]model.Scan, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT `+scanCols+` FROM scans ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Scan{}
	for rows.Next() {
		sc, err := scanScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// LastScan 返回最近一次扫描记录。
func (s *Store) LastScan() (*model.Scan, error) {
	sc, err := scanScan(s.db.QueryRow(`SELECT ` + scanCols + ` FROM scans ORDER BY started_at DESC, id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// GetScanByID 按主键读取扫描记录。
func (s *Store) GetScanByID(id int64) (*model.Scan, error) {
	sc, err := scanScan(s.db.QueryRow(`SELECT `+scanCols+` FROM scans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}
