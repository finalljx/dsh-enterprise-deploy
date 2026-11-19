// Postgres 持久化：用户注册表、活动状态、期望状态。
package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record 是一个用户的全部路由与活动事实。
type Record struct {
	Handle         string `json:"handle"`  // subject 的本地部分（如 alice），只用于展示与排序
	Subject        string `json:"subject"` // 规范身份（alice@corp）
	PublicHost     string `json:"publicHost"`
	PodIP          string `json:"podIP"`
	PrincipalURI   string `json:"principalUri"`
	LaunchToken    string `json:"launchToken"`
	DesiredState   string `json:"desiredState"` // running | sleeping | archived
	OpenTurn       bool   `json:"openTurn"`
	LastActivityAt int64  `json:"lastActivityAt"`
}

// ErrNotFound 表示用户不存在（区别于「存在但在睡觉」）。
var ErrNotFound = errors.New("record not found")

const recordColumns = `subject, handle, public_host,
	COALESCE(pod_ip, ''), COALESCE(principal_uri, ''), COALESCE(launch_token, ''),
	desired_state, open_turn, last_activity_at`

// Store 是 Postgres 实现；所有方法都接受调用方 context。
type Store struct{ pool *pgxpool.Pool }

// NewStore 建立连接池并校验连通性。
func NewStore(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 8
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close 释放连接池。
func (s *Store) Close() { s.pool.Close() }

// Ping 供健康检查使用。
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// UpsertUser 登记用户（开通时调用），不改动已有的活动状态。
func (s *Store) UpsertUser(ctx context.Context, r Record) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO dsh_users (subject, handle, public_host, principal_uri, desired_state, last_activity_at, registered_at)
VALUES ($1, $2, $3, $4, 'running', 0, 0)
ON CONFLICT (subject) DO UPDATE SET
  handle        = EXCLUDED.handle,
  public_host   = EXCLUDED.public_host,
  principal_uri = EXCLUDED.principal_uri,
  deleted_at    = NULL,   -- 管理员再次开通 = 显式复活（软删除的逆向操作）
  updated_at    = now()`,
		r.Subject, r.Handle, r.PublicHost, r.PrincipalURI)
	return err
}

// Put 由 pod 启动时上报，覆盖路由信息并把期望状态拉回 running。
func (s *Store) Put(ctx context.Context, r Record) error {
	now := time.Now().Unix()
	_, err := s.pool.Exec(ctx, `
INSERT INTO dsh_users
  (subject, handle, public_host, pod_ip, principal_uri, launch_token,
   desired_state, open_turn, last_activity_at, registered_at)
VALUES ($1, $2, $3, $4, $5, $6, 'running', false, $7, $7)
ON CONFLICT (subject) DO UPDATE SET
  handle           = EXCLUDED.handle,
  public_host      = EXCLUDED.public_host,
  pod_ip           = EXCLUDED.pod_ip,
  principal_uri    = EXCLUDED.principal_uri,
  launch_token     = EXCLUDED.launch_token,
  desired_state    = 'running',
  last_activity_at = GREATEST(dsh_users.last_activity_at, EXCLUDED.last_activity_at),
  registered_at    = EXCLUDED.registered_at,
  updated_at       = now()`,
		r.Subject, r.Handle, r.PublicHost, r.PodIP, r.PrincipalURI, r.LaunchToken, now)
	return err
}

// Touch 只更新活动字段，返回 false 表示用户未登记。
func (s *Store) Touch(ctx context.Context, subject string, openTurn bool, at int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE dsh_users
   SET open_turn        = $2,
       last_activity_at = GREATEST(last_activity_at, $3),
       updated_at       = now()
 WHERE subject = $1`, subject, openTurn, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// HandleOwner 返回 handle 已归属的 subject；空字符串表示未占用。
func (s *Store) HandleOwner(ctx context.Context, handle string) (string, error) {
	var subject string
	err := s.pool.QueryRow(ctx, `SELECT subject FROM dsh_users WHERE handle = $1`, handle).Scan(&subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return subject, err
}

// List 供管理台展示，按 handle 排序。
func (s *Store) List(ctx context.Context) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
	  FROM dsh_users WHERE deleted_at IS NULL ORDER BY handle LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// Get 读取一个用户。
func (s *Store) Get(ctx context.Context, subject string) (Record, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+recordColumns+`
	  FROM dsh_users WHERE subject = $1 AND deleted_at IS NULL`, subject)
	r, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, err
	}
	return r, nil
}

// Idle 返回可回收的用户：期望 running、无 open turn、静默超过阈值。
func (s *Store) Idle(ctx context.Context, olderThan time.Duration) ([]Record, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
	  FROM dsh_users
	 WHERE deleted_at IS NULL
	   AND desired_state = 'running'
	   AND open_turn = false
	   AND pod_ip IS NOT NULL
	   AND last_activity_at < $1
	 ORDER BY last_activity_at ASC
	 LIMIT 500`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// Wanted 返回期望为 running 的用户，供回收器反向拉起。
func (s *Store) Wanted(ctx context.Context) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
	  FROM dsh_users
	 WHERE deleted_at IS NULL
	   AND desired_state = 'running'
	 ORDER BY handle
	 LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// SetDesired 修改期望状态。
func (s *Store) SetDesired(ctx context.Context, subject, state string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE dsh_users SET desired_state = $2, updated_at = now() WHERE subject = $1`, subject, state)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClearPod 在缩容后清掉已失效的 pod 信息，避免 router 继续转发。
func (s *Store) ClearPod(ctx context.Context, subject string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE dsh_users
   SET pod_ip = NULL, launch_token = NULL, open_turn = false, updated_at = now()
 WHERE subject = $1`, subject)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanRecord(row scanner) (Record, error) {
	var r Record
	err := row.Scan(&r.Subject, &r.Handle, &r.PublicHost, &r.PodIP,
		&r.PrincipalURI, &r.LaunchToken, &r.DesiredState, &r.OpenTurn, &r.LastActivityAt)
	return r, err
}

func collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]Record, error) {
	out := make([]Record, 0, 16)
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SoftDelete 软删除：保留数据，但对 router / 回收器立即不可见。
func (s *Store) SoftDelete(ctx context.Context, subject string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE dsh_users
   SET deleted_at = now(), desired_state = 'sleeping',
       pod_ip = NULL, launch_token = NULL, open_turn = false, updated_at = now()
 WHERE subject = $1 AND deleted_at IS NULL`, subject)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Purge 硬删除：只用于软删除之后的定期清理，或管理员显式要求彻底抹掉。
func (s *Store) Purge(ctx context.Context, subject string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dsh_users WHERE subject = $1`, subject)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeletedBefore 返回软删除超过阈值的用户，供定期清理使用。
func (s *Store) DeletedBefore(ctx context.Context, olderThan time.Duration) ([]Record, error) {
	cutoff := time.Now().Add(-olderThan)
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
	  FROM dsh_users
	 WHERE deleted_at IS NOT NULL AND deleted_at < $1
	 ORDER BY deleted_at ASC
	 LIMIT 500`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}
