// Package store 提供基于 SQLite 的持久化层。
//
// 选用纯 Go 的 modernc.org/sqlite 驱动，避免 CGO，
// 这样交叉编译 linux/amd64 与 linux/arm64 都不需要目标平台工具链。
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 数据库句柄。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并完成建表。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 写操作串行化，单连接可彻底规避 database is locked。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// migrate 建表。全部使用 IF NOT EXISTS，可重复执行。
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS images (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			reference       TEXT    NOT NULL,
			container_id    TEXT    NOT NULL DEFAULT '',
			container_name  TEXT    NOT NULL DEFAULT '',
			origin          TEXT    NOT NULL DEFAULT '',
			image_id        TEXT    NOT NULL DEFAULT '',
			size_bytes      INTEGER NOT NULL DEFAULT 0,
			image_created   INTEGER NOT NULL DEFAULT 0,
			dangling        INTEGER NOT NULL DEFAULT 0,
			local_digest    TEXT    NOT NULL DEFAULT '',
			remote_digest   TEXT    NOT NULL DEFAULT '',
			local_tag       TEXT    NOT NULL DEFAULT '',
			latest_tag      TEXT    NOT NULL DEFAULT '',
			aliases         TEXT    NOT NULL DEFAULT '',
			mode            TEXT    NOT NULL DEFAULT 'auto',
			mode_override   INTEGER NOT NULL DEFAULT 0,
			ignored         INTEGER NOT NULL DEFAULT 0,
			ignored_digest  TEXT    NOT NULL DEFAULT '',
			status          TEXT    NOT NULL DEFAULT 'unknown',
			baseline_at     INTEGER NOT NULL DEFAULT 0,
			last_checked_at INTEGER NOT NULL DEFAULT 0,
			last_error      TEXT    NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL DEFAULT 0,
			updated_at      INTEGER NOT NULL DEFAULT 0,
			UNIQUE(reference, container_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_images_container ON images(container_id)`,

		`CREATE TABLE IF NOT EXISTS notifications (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			image_ref         TEXT    NOT NULL,
			container_id      TEXT    NOT NULL DEFAULT '',
			container_name    TEXT    NOT NULL DEFAULT '',
			kind              TEXT    NOT NULL,
			title             TEXT    NOT NULL DEFAULT '',
			body              TEXT    NOT NULL DEFAULT '',
			local_summary     TEXT    NOT NULL DEFAULT '',
			remote_summary    TEXT    NOT NULL DEFAULT '',
			local_tag         TEXT    NOT NULL DEFAULT '',
			latest_tag        TEXT    NOT NULL DEFAULT '',
			aliases           TEXT    NOT NULL DEFAULT '',
			read              INTEGER NOT NULL DEFAULT 0,
			feishu_message_id TEXT    NOT NULL DEFAULT '',
			channel           TEXT    NOT NULL DEFAULT 'feishu',
			chat_ref          TEXT    NOT NULL DEFAULT '',
			message_id        TEXT    NOT NULL DEFAULT '',
			card_state        TEXT    NOT NULL DEFAULT 'pending',
			deliveries        TEXT    NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL DEFAULT 0,
			updated_at        INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_created ON notifications(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_ref ON notifications(image_ref, container_id)`,

		`CREATE TABLE IF NOT EXISTS tasks (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			notification_id  INTEGER NOT NULL DEFAULT 0,
			image_ref        TEXT    NOT NULL DEFAULT '',
			container_id     TEXT    NOT NULL DEFAULT '',
			container_name   TEXT    NOT NULL DEFAULT '',
			new_container_id TEXT    NOT NULL DEFAULT '',
			status           TEXT    NOT NULL DEFAULT 'pending',
			triggered_by     TEXT    NOT NULL DEFAULT '',
			log              TEXT    NOT NULL DEFAULT '',
			error            TEXT    NOT NULL DEFAULT '',
			backup_name      TEXT    NOT NULL DEFAULT '',
			started_at       INTEGER NOT NULL DEFAULT 0,
			finished_at      INTEGER NOT NULL DEFAULT 0,
			created_at       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at DESC)`,

		`CREATE TABLE IF NOT EXISTS scans (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			started_at  INTEGER NOT NULL DEFAULT 0,
			finished_at INTEGER NOT NULL DEFAULT 0,
			total       INTEGER NOT NULL DEFAULT 0,
			with_update INTEGER NOT NULL DEFAULT 0,
			notified    INTEGER NOT NULL DEFAULT 0,
			forced      INTEGER NOT NULL DEFAULT 0,
			error       TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scans_started ON scans(started_at DESC)`,

		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	return s.addMissingColumns()
}

// imageAddedColumns 是 1.1 起为 images 表新增的列。
//
// CREATE TABLE IF NOT EXISTS 对已存在的表一个字段都不会补，
// 而缺列的后果是整个查询在 SELECT 阶段就报 no such column —— 页面直接白掉。
// 所以老库必须逐个 ALTER TABLE 补齐。
var imageAddedColumns = []struct{ name, ddl string }{
	{"origin", "TEXT NOT NULL DEFAULT ''"},
	{"image_id", "TEXT NOT NULL DEFAULT ''"},
	{"size_bytes", "INTEGER NOT NULL DEFAULT 0"},
	{"image_created", "INTEGER NOT NULL DEFAULT 0"},
	{"dangling", "INTEGER NOT NULL DEFAULT 0"},
	// 用户在飞书卡片上「忽略本版本」时记下的远端摘要，见 model.Image.IgnoredDigest。
	{"ignored_digest", "TEXT NOT NULL DEFAULT ''"},
}

// notificationAddedColumns 是 1.2.4 起为 notifications 表新增的列。
//
// 加了 Telegram 之后，「把消息 ID 记在哪」不能再叫 feishu_message_id：
// 更新完成后要按渠道去改原来那条消息，而两个渠道的定位方式不一样。
// 老库的 feishu_message_id 由 addMissingColumns 回填到 message_id，不会丢历史。
var notificationAddedColumns = []struct{ name, ddl string }{
	{"channel", "TEXT NOT NULL DEFAULT 'feishu'"},
	{"chat_ref", "TEXT NOT NULL DEFAULT ''"},
	{"message_id", "TEXT NOT NULL DEFAULT ''"},
	// 全量投递记录（JSON 数组）。同时配了两个渠道时一条提醒要各发一份，
	// 只存一个 message_id 就只能改得动其中一个，另一个会一直停在「待更新」。
	{"deliveries", "TEXT NOT NULL DEFAULT ''"},
}

// addMissingColumns 给老库补上新增的列。
func (s *Store) addMissingColumns() error {
	if err := s.addImageColumns(); err != nil {
		return err
	}
	return s.addNotificationColumns()
}

func (s *Store) addImageColumns() error {
	have, err := s.tableColumns("images")
	if err != nil {
		return err
	}
	needBackfill := !have["origin"]
	for _, col := range imageAddedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE images ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return fmt.Errorf("补列 %s 失败: %w", col.name, err)
		}
	}
	// 老库的记录没有来源信息，按「有没有容器」回填一次。
	// 少了这一步，升级后原来那些容器监控项会统统跑到「镜像」标签页里去。
	if needBackfill {
		if _, err := s.db.Exec(
			`UPDATE images SET origin = CASE WHEN container_id = '' THEN 'watch' ELSE 'container' END WHERE origin = ''`,
		); err != nil {
			return fmt.Errorf("回填来源失败: %w", err)
		}
	}
	return nil
}

func (s *Store) addNotificationColumns() error {
	have, err := s.tableColumns("notifications")
	if err != nil {
		return err
	}
	for _, col := range notificationAddedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE notifications ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return fmt.Errorf("补列 %s 失败: %w", col.name, err)
		}
	}
	// 升级前的通知都只发过飞书，把旧列的值搬到新列，
	// 否则升级后那些还挂着的卡片点按钮能收到回调，但状态改不回去。
	if have["feishu_message_id"] && !have["message_id"] {
		if _, err := s.db.Exec(
			`UPDATE notifications SET channel = 'feishu', message_id = feishu_message_id WHERE message_id = ''`,
		); err != nil {
			return fmt.Errorf("回填消息 ID 失败: %w", err)
		}
	}
	return nil
}

// tableColumns 读取一张表已有的列名。
func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ---- 时间与布尔值转换（统一用 unix 秒与 0/1 存储，避免驱动差异） ----

func tsToTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

func timeToTs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func tsToTimePtr(v int64) *time.Time {
	if v == 0 {
		return nil
	}
	t := time.Unix(v, 0)
	return &t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetSetting 读取单个设置项，不存在时返回空串与 false。
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting 写入单个设置项。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// AllSettings 返回全部设置项。
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
