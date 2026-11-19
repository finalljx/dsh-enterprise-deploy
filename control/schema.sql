-- DSH 控制面schema。幂等，可重复执行。
CREATE TABLE IF NOT EXISTS dsh_users (
  subject          text PRIMARY KEY,              -- 规范身份，alice@corp
  handle           text NOT NULL,                 -- K8s 标签值，alice
  public_host      text NOT NULL,                 -- alice.dsh.corp
  pod_ip           text,
  principal_uri    text,                          -- spiffe://corp/dsh/user/alice
  launch_token     text,
  desired_state    text NOT NULL DEFAULT 'running'
                   CHECK (desired_state IN ('running','sleeping','archived')),
  open_turn        boolean NOT NULL DEFAULT false,
  last_activity_at bigint  NOT NULL DEFAULT 0,
  registered_at    bigint  NOT NULL DEFAULT 0,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  deleted_at       timestamptz                  -- 软删除：非空即对 router/回收器不可见
);

-- 一人一 pod：handle 唯一
CREATE UNIQUE INDEX IF NOT EXISTS dsh_users_handle_uniq ON dsh_users (handle);

-- 回收扫描路径
CREATE INDEX IF NOT EXISTS dsh_users_idle_idx
  ON dsh_users (desired_state, open_turn, last_activity_at);

-- 反向拉起扫描路径
CREATE INDEX IF NOT EXISTS dsh_users_wanted_idx ON dsh_users (desired_state, handle);

-- 已有库升级（幂等）：软删除列
ALTER TABLE dsh_users ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
CREATE INDEX IF NOT EXISTS dsh_users_deleted_idx
  ON dsh_users (deleted_at) WHERE deleted_at IS NOT NULL;
