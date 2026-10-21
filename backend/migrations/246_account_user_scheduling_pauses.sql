-- 规则按真实账号与用户 ID 持久化；账号复制或邮箱重新注册不会继承其他 ID 的名单。
CREATE TABLE IF NOT EXISTS account_user_scheduling_pauses (
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_admin_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    PRIMARY KEY (account_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_account_user_scheduling_pauses_user_account
    ON account_user_scheduling_pauses (user_id, account_id);
