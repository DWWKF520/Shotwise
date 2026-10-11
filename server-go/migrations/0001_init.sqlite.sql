-- 0001_init 的 SQLite 差异部分：与 0001_init.sql 同属一个版本，由执行器一并应用。
--
-- SQLite 布尔列存 1/0，部分索引谓词只能按整数比较（PG 的 is_active 是 boolean，
-- 写 = 1 会报类型不匹配），因此这类索引不放在通用文件里。
-- 整型主键靠 SQLite 的 rowid 别名自增，无需额外语句。
-- +migrate Up
CREATE UNIQUE INDEX uq_agent_credential_one_active_per_user_sdk ON agent_anthropic_credentials (user_id, sdk_type) WHERE is_active = 1;
CREATE UNIQUE INDEX uq_provider_credential_one_active ON provider_credential (provider) WHERE is_active = 1;
