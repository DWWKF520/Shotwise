-- 0001_init：SHOTWISE 数据库基线（53 张业务表）。
--
-- 来源是既有 projects/.shotwise.db 的实测 DDL，不是 78 个 Alembic 迁移的复刻：
-- 迁移链里大量步骤是历史事故修补，对新库无意义，且 ORM 与 DB 已存在漂移，
-- 权威描述只有实测 DDL。表顺序按外键依赖拓扑排序，PostgreSQL 也可顺序建。
--
-- 类型口径：FLOAT 统一为 DOUBLE PRECISION（PG 的 FLOAT 是 4 字节，会丢 Python
-- float64 的精度）；布尔默认值写 true/false（SQLite 3.23+ 与 PG 都接受）；
-- 时间列统一为 TIMESTAMPTZ（实测库混用 VARCHAR/DATETIME，新库不再跟随，
-- 读取侧由 db.FlexibleTime 兼容既有库的任意形态）。
-- 方言独有部分放 0001_init.sqlite.sql / 0001_init.pg.sql，与本文件同属 0001 版本。
-- +migrate Up
CREATE TABLE "agent_anthropic_credentials" (
    id INTEGER NOT NULL,
    user_id VARCHAR(64) DEFAULT 'default' NOT NULL,
    preset_id VARCHAR(64) NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    base_url TEXT NOT NULL,
    api_key TEXT NOT NULL,
    model VARCHAR(128),
    haiku_model VARCHAR(128),
    sonnet_model VARCHAR(128),
    opus_model VARCHAR(128),
    subagent_model VARCHAR(128),
    is_active BOOLEAN DEFAULT false NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    sdk_type VARCHAR(16) DEFAULT 'claude' NOT NULL,
    model_map JSON,
    protocol VARCHAR(32) DEFAULT 'chat_completions' NOT NULL,
    PRIMARY KEY (id)
);
CREATE TABLE "users" (
    id VARCHAR NOT NULL,
    username VARCHAR NOT NULL,
    role VARCHAR DEFAULT 'user' NOT NULL,
    is_active BOOLEAN DEFAULT true NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    github_id VARCHAR,
    PRIMARY KEY (id),
    CONSTRAINT uq_users_github_id UNIQUE (github_id),
    UNIQUE (username)
);
CREATE TABLE "agent_session_entries" (
    project_key VARCHAR NOT NULL,
    session_id VARCHAR NOT NULL,
    subpath VARCHAR DEFAULT '' NOT NULL,
    seq BIGINT NOT NULL,
    uuid VARCHAR,
    entry_type VARCHAR NOT NULL,
    payload JSON NOT NULL,
    mtime_ms BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    PRIMARY KEY (project_key, session_id, subpath, seq),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "agent_session_event_log" (
    session_id VARCHAR NOT NULL,
    seq BIGINT NOT NULL,
    entry_type VARCHAR NOT NULL,
    payload JSON NOT NULL,
    client_key VARCHAR,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    PRIMARY KEY (session_id, seq),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "agent_session_summaries" (
    project_key VARCHAR NOT NULL,
    session_id VARCHAR NOT NULL,
    mtime_ms BIGINT NOT NULL,
    data JSON NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    PRIMARY KEY (project_key, session_id),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "agent_session_user_message_links" (
    session_id VARCHAR NOT NULL,
    user_entry_uuid VARCHAR NOT NULL,
    sdk_entry_uuid VARCHAR NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    PRIMARY KEY (session_id, user_entry_uuid),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "agent_sessions" (
    id VARCHAR NOT NULL,
    sdk_session_id VARCHAR NOT NULL,
    project_name VARCHAR NOT NULL,
    title VARCHAR DEFAULT ('') NOT NULL,
    status VARCHAR DEFAULT 'idle' NOT NULL,
    created_at VARCHAR NOT NULL,
    updated_at VARCHAR NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    sdk_type VARCHAR(16) DEFAULT 'claude' NOT NULL,
    claude_resume_id VARCHAR,
    superseded_by VARCHAR,
    fork_parent_session_id VARCHAR,
    fork_anchor_uuid VARCHAR,
    PRIMARY KEY (id),
    CONSTRAINT uq_agent_sessions_sdk_session_id UNIQUE (sdk_session_id),
    CONSTRAINT fk_agent_sessions_user_id FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "api_calls" (
    id INTEGER NOT NULL,
    project_name VARCHAR NOT NULL,
    call_type VARCHAR NOT NULL,
    model VARCHAR NOT NULL,
    prompt TEXT,
    resolution VARCHAR,
    duration_seconds INTEGER,
    aspect_ratio VARCHAR,
    generate_audio BOOLEAN DEFAULT true,
    status VARCHAR DEFAULT 'pending' NOT NULL,
    error_message TEXT,
    output_path TEXT,
    started_at VARCHAR NOT NULL,
    finished_at VARCHAR,
    duration_ms INTEGER,
    retry_count INTEGER DEFAULT '0' NOT NULL,
    cost_amount DOUBLE PRECISION DEFAULT '0.0' NOT NULL,
    created_at VARCHAR NOT NULL,
    currency VARCHAR DEFAULT 'USD' NOT NULL,
    provider VARCHAR DEFAULT 'gemini' NOT NULL,
    usage_tokens INTEGER,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    input_tokens INTEGER,
    output_tokens INTEGER,
    segment_id VARCHAR(20),
    image_input_tokens INTEGER,
    image_output_tokens INTEGER,
    text_input_tokens INTEGER,
    text_output_tokens INTEGER,
    task_id VARCHAR,
    purpose VARCHAR,
    session_id VARCHAR,
    inputs JSON,
    error_code VARCHAR,
    error_params JSON,
    PRIMARY KEY (id),
    CONSTRAINT fk_api_calls_user_id FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "api_keys" (
    id INTEGER NOT NULL,
    name VARCHAR NOT NULL,
    key_hash VARCHAR NOT NULL,
    key_prefix VARCHAR NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT fk_api_keys_user_id FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE,
    UNIQUE (name),
    UNIQUE (key_hash)
);
CREATE TABLE "assembly_plans" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    project_name VARCHAR(200) NOT NULL,
    scope VARCHAR(32) DEFAULT 'episode' NOT NULL,
    episode_number INTEGER,
    name VARCHAR(200) NOT NULL,
    status VARCHAR(24) DEFAULT 'draft' NOT NULL,
    current_revision_number INTEGER DEFAULT '1' NOT NULL,
    current_source_fingerprint VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    preview_revision_number INTEGER,
    preview_ready_at TIMESTAMPTZ,
    render_confirmed_by VARCHAR(128),
    render_confirmed_at TIMESTAMPTZ,
    preview_confirmed_by VARCHAR(128),
    preview_confirmed_at TIMESTAMPTZ,
    PRIMARY KEY (id)
);
CREATE TABLE "assembly_plan_revisions" (
    id VARCHAR(36) NOT NULL,
    plan_id VARCHAR(36) NOT NULL,
    version_number INTEGER NOT NULL,
    source_fingerprint VARCHAR(64) NOT NULL,
    source_snapshot_json TEXT NOT NULL,
    timeline_json TEXT NOT NULL,
    audio_json TEXT NOT NULL,
    subtitle_json TEXT NOT NULL,
    packaging_json TEXT NOT NULL,
    output_profile_json TEXT NOT NULL,
    validation_json TEXT NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(plan_id) REFERENCES assembly_plans (id) ON DELETE CASCADE,
    CONSTRAINT uq_assembly_plan_revision_number UNIQUE (plan_id, version_number)
);
CREATE TABLE "assets" (
    id VARCHAR(36) NOT NULL,
    type VARCHAR(32) NOT NULL,
    name VARCHAR(200) NOT NULL,
    description TEXT NOT NULL,
    voice_style TEXT NOT NULL,
    image_path VARCHAR(500),
    source_project VARCHAR(200),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    audio_path VARCHAR(500),
    PRIMARY KEY (id),
    CONSTRAINT uq_asset_type_name UNIQUE (type, name)
);
CREATE TABLE "workflow_definitions" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    workspace_id VARCHAR(128) NOT NULL,
    project_id VARCHAR(200) NOT NULL,
    name VARCHAR(200) NOT NULL,
    scope VARCHAR(32) DEFAULT 'project' NOT NULL,
    active_revision_id VARCHAR(36),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "workflow_revisions" (
    id VARCHAR(36) NOT NULL,
    definition_id VARCHAR(36) NOT NULL,
    revision_no INTEGER NOT NULL,
    status VARCHAR(24) DEFAULT 'draft' NOT NULL,
    graph_hash VARCHAR(64) NOT NULL,
    execution_hash VARCHAR(64) NOT NULL,
    template_lock_json TEXT,
    created_by VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    content_mode VARCHAR(32) DEFAULT 'drama' NOT NULL,
    generation_mode VARCHAR(32) DEFAULT 'storyboard' NOT NULL,
    input_schema_json TEXT DEFAULT '{}' NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_workflow_revision_number UNIQUE (definition_id, revision_no),
    CONSTRAINT ck_workflow_revision_number_positive CHECK (revision_no > 0),
    CONSTRAINT ck_workflow_revision_content_mode CHECK (content_mode IN ('drama', 'narration', 'ad', 'manga')),
    CONSTRAINT ck_workflow_revision_generation_mode CHECK (generation_mode IN ('storyboard', 'reference_video')),
    CONSTRAINT ck_workflow_revision_graph_hash_nonempty CHECK (length(graph_hash) > 0),
    CONSTRAINT ck_workflow_revision_execution_hash_nonempty CHECK (length(execution_hash) > 0),
    FOREIGN KEY(definition_id) REFERENCES workflow_definitions (id) ON DELETE CASCADE
);
CREATE TABLE "workflow_runs" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    workflow_revision_id VARCHAR(36) NOT NULL,
    workspace_id VARCHAR(128) NOT NULL,
    project_id VARCHAR(200) NOT NULL,
    script_revision_id VARCHAR(128),
    status VARCHAR(24) DEFAULT 'planned' NOT NULL,
    mode VARCHAR(24) DEFAULT 'hybrid' NOT NULL,
    execution_hash VARCHAR(64) NOT NULL,
    input_fingerprint VARCHAR(64) NOT NULL,
    graph_snapshot_ref VARCHAR(200),
    input_snapshot_json TEXT DEFAULT '{}' NOT NULL,
    progress DOUBLE PRECISION,
    version INTEGER DEFAULT '1' NOT NULL,
    control_generation INTEGER DEFAULT '0' NOT NULL,
    trace_id VARCHAR(64) NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    episode_id VARCHAR(128),
    budget_limit DOUBLE PRECISION,
    spent_amount DOUBLE PRECISION DEFAULT '0' NOT NULL,
    reserved_amount DOUBLE PRECISION DEFAULT '0' NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT ck_workflow_run_budget_limit_nonnegative CHECK (budget_limit IS NULL OR budget_limit >= 0),
    CONSTRAINT ck_workflow_run_spent_nonnegative CHECK (spent_amount >= 0),
    CONSTRAINT ck_workflow_run_reserved_nonnegative CHECK (reserved_amount >= 0),
    CONSTRAINT ck_workflow_run_budget_not_exceeded CHECK (budget_limit IS NULL OR spent_amount + reserved_amount <= budget_limit),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE,
    FOREIGN KEY(workflow_revision_id) REFERENCES workflow_revisions (id)
);
CREATE TABLE "workflow_node_runs" (
    id VARCHAR(36) NOT NULL,
    workflow_run_id VARCHAR(36) NOT NULL,
    node_key VARCHAR(128) NOT NULL,
    attempt_no INTEGER DEFAULT '1' NOT NULL,
    status VARCHAR(24) DEFAULT 'blocked' NOT NULL,
    input_hash VARCHAR(64),
    input_snapshot_json TEXT,
    progress DOUBLE PRECISION,
    progress_source VARCHAR(32),
    phase_code VARCHAR(64),
    phase_params_json TEXT,
    error_code VARCHAR(64),
    error_params_json TEXT,
    output_refs_json TEXT,
    lease_owner VARCHAR(128),
    lease_until TIMESTAMPTZ,
    fencing_token INTEGER DEFAULT '0' NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(workflow_run_id) REFERENCES workflow_runs (id) ON DELETE CASCADE,
    CONSTRAINT uq_workflow_node_run_attempt UNIQUE (workflow_run_id, node_key, attempt_no)
);
CREATE TABLE "budget_reservations" (
    id VARCHAR(36) NOT NULL,
    workspace_id VARCHAR(128) NOT NULL,
    workflow_run_id VARCHAR(36) NOT NULL,
    node_run_id VARCHAR(36),
    currency VARCHAR(8) DEFAULT 'USD' NOT NULL,
    estimated_amount DOUBLE PRECISION DEFAULT '0' NOT NULL,
    reserved_amount DOUBLE PRECISION DEFAULT '0' NOT NULL,
    settled_amount DOUBLE PRECISION DEFAULT '0' NOT NULL,
    price_catalog_version VARCHAR(64),
    status VARCHAR(24) DEFAULT 'reserved' NOT NULL,
    expires_at TIMESTAMPTZ,
    PRIMARY KEY (id),
    FOREIGN KEY(workflow_run_id) REFERENCES workflow_runs (id) ON DELETE CASCADE,
    FOREIGN KEY(node_run_id) REFERENCES workflow_node_runs (id) ON DELETE CASCADE
);
CREATE TABLE "creation_compatibility_events" (
    id VARCHAR(36) NOT NULL,
    creation_skill_version_id VARCHAR(128) NOT NULL,
    project_content_mode VARCHAR(32) NOT NULL,
    project_generation_mode VARCHAR(32) NOT NULL,
    resource_types_json TEXT DEFAULT '[]' NOT NULL,
    reason VARCHAR(128) NOT NULL,
    outcome VARCHAR(32) DEFAULT 'unresolved' NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id)
);
CREATE TABLE "creation_plans" (
    id VARCHAR(80) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    workspace_id VARCHAR(128) NOT NULL,
    project_id VARCHAR(200) NOT NULL,
    creation_skill_version_id VARCHAR(128) NOT NULL,
    skill_id VARCHAR(128) NOT NULL,
    workflow_revision VARCHAR(128) NOT NULL,
    fingerprint VARCHAR(64) NOT NULL,
    idempotency_key VARCHAR(256) NOT NULL,
    project_snapshot_json TEXT NOT NULL,
    resource_ids_json TEXT DEFAULT '[]' NOT NULL,
    parameters_json TEXT DEFAULT '{}' NOT NULL,
    preview_json TEXT NOT NULL,
    estimated_cost DOUBLE PRECISION DEFAULT '0' NOT NULL,
    status VARCHAR(24) DEFAULT 'previewed' NOT NULL,
    workflow_run_id VARCHAR(36),
    created_by VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    PRIMARY KEY (id),
    CONSTRAINT uq_creation_plan_idempotency UNIQUE (user_id, project_id, idempotency_key),
    UNIQUE (fingerprint),
    FOREIGN KEY(workflow_run_id) REFERENCES workflow_runs (id) ON DELETE SET NULL
);
CREATE TABLE "creation_skill_definitions" (
    id VARCHAR(128) NOT NULL,
    slug VARCHAR(128) NOT NULL,
    official BOOLEAN DEFAULT true NOT NULL,
    active BOOLEAN DEFAULT true NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (slug)
);
CREATE TABLE "creation_skill_versions" (
    id VARCHAR(128) NOT NULL,
    skill_id VARCHAR(128) NOT NULL,
    version INTEGER NOT NULL,
    title VARCHAR(200) NOT NULL,
    summary TEXT NOT NULL,
    category VARCHAR(64) NOT NULL,
    workflow_template_revision_alias VARCHAR(200) NOT NULL,
    expected_outputs_json TEXT DEFAULT '[]' NOT NULL,
    review_required BOOLEAN DEFAULT false NOT NULL,
    estimated_cost_hint VARCHAR(128),
    status VARCHAR(24) DEFAULT 'published' NOT NULL,
    frozen_at TIMESTAMPTZ NOT NULL,
    workflow_revision_id VARCHAR(36),
    PRIMARY KEY (id),
    CONSTRAINT uq_creation_skill_version_number UNIQUE (skill_id, version),
    CONSTRAINT fk_creation_skill_versions_workflow_revision FOREIGN KEY(workflow_revision_id) REFERENCES workflow_revisions (id) ON DELETE RESTRICT,
    CONSTRAINT ck_creation_skill_version_published_workflow_revision CHECK (status <> 'published' OR (workflow_revision_id IS NOT NULL AND length(trim(workflow_revision_id)) > 0)),
    FOREIGN KEY(skill_id) REFERENCES creation_skill_definitions (id) ON DELETE CASCADE
);
CREATE TABLE "creation_skill_compatibilities" (
    skill_version_id VARCHAR(128) NOT NULL,
    content_modes_json TEXT DEFAULT '[]' NOT NULL,
    generation_modes_json TEXT DEFAULT '[]' NOT NULL,
    required_inputs_json TEXT DEFAULT '[]' NOT NULL,
    scopes_json TEXT DEFAULT '[]' NOT NULL,
    grid_storyboards_json TEXT,
    PRIMARY KEY (skill_version_id),
    FOREIGN KEY(skill_version_id) REFERENCES creation_skill_versions (id) ON DELETE CASCADE
);
CREATE TABLE "creative_boards" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    project_id VARCHAR(200) NOT NULL,
    name VARCHAR(200) NOT NULL,
    viewport_json TEXT DEFAULT '{}' NOT NULL,
    display_settings_json TEXT DEFAULT '{}' NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    revision INTEGER DEFAULT '1' NOT NULL,
    PRIMARY KEY (id)
);
CREATE TABLE "creative_board_items" (
    id VARCHAR(36) NOT NULL,
    board_id VARCHAR(36) NOT NULL,
    item_type VARCHAR(32) NOT NULL,
    resource_type VARCHAR(32) NOT NULL,
    resource_id VARCHAR(256) NOT NULL,
    position_x INTEGER DEFAULT '0' NOT NULL,
    position_y INTEGER DEFAULT '0' NOT NULL,
    width INTEGER DEFAULT '280' NOT NULL,
    height INTEGER DEFAULT '180' NOT NULL,
    group_id VARCHAR(36),
    display_settings_json TEXT DEFAULT '{}' NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(board_id) REFERENCES creative_boards (id) ON DELETE CASCADE
);
CREATE TABLE "creative_board_edges" (
    id VARCHAR(36) NOT NULL,
    board_id VARCHAR(36) NOT NULL,
    source_item_id VARCHAR(36) NOT NULL,
    target_item_id VARCHAR(36) NOT NULL,
    relation VARCHAR(32) NOT NULL,
    ordinal INTEGER,
    metadata_json TEXT DEFAULT '{}' NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_creative_board_edge UNIQUE (board_id, source_item_id, target_item_id, relation),
    FOREIGN KEY(board_id) REFERENCES creative_boards (id) ON DELETE CASCADE,
    FOREIGN KEY(source_item_id) REFERENCES creative_board_items (id) ON DELETE CASCADE,
    FOREIGN KEY(target_item_id) REFERENCES creative_board_items (id) ON DELETE CASCADE
);
CREATE TABLE "creative_board_versions" (
    id VARCHAR(36) NOT NULL,
    board_id VARCHAR(36) NOT NULL,
    version_number INTEGER NOT NULL,
    version_name VARCHAR(200) NOT NULL,
    snapshot_json TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_creative_board_version_number UNIQUE (board_id, version_number),
    FOREIGN KEY(board_id) REFERENCES creative_boards (id) ON DELETE CASCADE
);
CREATE TABLE "custom_provider" (
    id INTEGER NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    base_url TEXT NOT NULL,
    api_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    discovery_format VARCHAR(32) NOT NULL,
    image_max_workers INTEGER,
    video_max_workers INTEGER,
    audio_max_workers INTEGER,
    is_enabled BOOLEAN DEFAULT true NOT NULL,
    image_request_timeout_seconds DOUBLE PRECISION,
    PRIMARY KEY (id),
    CONSTRAINT ck_custom_provider_video_max_workers_positive CHECK (video_max_workers IS NULL OR video_max_workers >= 1),
    CONSTRAINT ck_custom_provider_audio_max_workers_positive CHECK (audio_max_workers IS NULL OR audio_max_workers >= 1),
    CONSTRAINT ck_custom_provider_image_max_workers_positive CHECK (image_max_workers IS NULL OR image_max_workers >= 1),
    CONSTRAINT ck_custom_provider_image_request_timeout_positive CHECK (image_request_timeout_seconds IS NULL OR image_request_timeout_seconds > 0)
);
CREATE TABLE "custom_provider_model" (
    id INTEGER NOT NULL,
    provider_id INTEGER NOT NULL,
    model_id VARCHAR(128) NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    is_default BOOLEAN NOT NULL,
    is_enabled BOOLEAN NOT NULL,
    price_unit VARCHAR(16),
    price_input DOUBLE PRECISION,
    price_output DOUBLE PRECISION,
    currency VARCHAR(8),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    supported_durations TEXT,
    resolution VARCHAR(64),
    endpoint VARCHAR(32) NOT NULL,
    capability_overrides JSON,
    endpoint_declaration JSON,
    PRIMARY KEY (id),
    CONSTRAINT fk_custom_provider_model_provider_id FOREIGN KEY(provider_id) REFERENCES custom_provider (id) ON DELETE CASCADE,
    CONSTRAINT uq_custom_provider_model UNIQUE (provider_id, model_id)
);
CREATE TABLE "tasks" (
    task_id VARCHAR NOT NULL,
    project_name VARCHAR NOT NULL,
    task_type VARCHAR NOT NULL,
    media_type VARCHAR NOT NULL,
    resource_id VARCHAR NOT NULL,
    script_file VARCHAR,
    payload_json TEXT,
    status VARCHAR NOT NULL,
    result_json TEXT,
    error_message TEXT,
    source VARCHAR DEFAULT 'webui' NOT NULL,
    dependency_task_id VARCHAR,
    dependency_group VARCHAR,
    dependency_index INTEGER,
    queued_at VARCHAR NOT NULL,
    started_at VARCHAR,
    finished_at VARCHAR,
    updated_at VARCHAR NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    cancelled_by VARCHAR,
    provider_id VARCHAR,
    provider_job_id VARCHAR,
    resource_type VARCHAR,
    provider_endpoint VARCHAR,
    workflow_run_id VARCHAR(36),
    workflow_node_run_id VARCHAR(36),
    workflow_node_run_item_id VARCHAR(36),
    input_fingerprint VARCHAR(64),
    request_idempotency_key VARCHAR(256),
    progress DOUBLE PRECISION,
    progress_source VARCHAR(32),
    phase_code VARCHAR(64),
    lease_owner VARCHAR(128),
    lease_until TIMESTAMPTZ,
    fencing_token INTEGER DEFAULT '0' NOT NULL,
    submitted_base_url VARCHAR,
    execution_checkpoint_json TEXT,
    PRIMARY KEY (task_id),
    CONSTRAINT fk_tasks_user_id FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "external_executions" (
    id VARCHAR(36) NOT NULL,
    task_id VARCHAR(36) NOT NULL,
    provider_id VARCHAR(128) NOT NULL,
    provider_account_id VARCHAR(128),
    endpoint_snapshot TEXT,
    model_snapshot TEXT,
    provider_job_id VARCHAR(256),
    provider_idempotency_key VARCHAR(256),
    submit_state VARCHAR(32) DEFAULT 'prepared' NOT NULL,
    remote_state VARCHAR(64),
    cancel_state VARCHAR(64),
    request_hash VARCHAR(64),
    response_digest VARCHAR(64),
    fencing_token INTEGER DEFAULT '0' NOT NULL,
    submitted_at TIMESTAMPTZ,
    last_polled_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    PRIMARY KEY (id),
    FOREIGN KEY(task_id) REFERENCES tasks (task_id) ON DELETE CASCADE,
    UNIQUE (task_id)
);
CREATE TABLE "generation_batches" (
    batch_id VARCHAR NOT NULL,
    project_name VARCHAR NOT NULL,
    admission_state VARCHAR NOT NULL,
    items_json TEXT NOT NULL,
    task_ids_json TEXT NOT NULL,
    cancel_requested BOOLEAN DEFAULT false NOT NULL,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    PRIMARY KEY (batch_id),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "media_assets" (
    id VARCHAR(36) NOT NULL,
    project_id VARCHAR(255) NOT NULL,
    kind VARCHAR(32) NOT NULL,
    original_name VARCHAR(512) NOT NULL,
    mime_type VARCHAR(255),
    extension VARCHAR(32) NOT NULL,
    physical_path TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    fingerprint VARCHAR(128) NOT NULL,
    origin VARCHAR(32) NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    width INTEGER,
    height INTEGER,
    duration_seconds DOUBLE PRECISION,
    workflow_run_id VARCHAR(255),
    workflow_node_key VARCHAR(255),
    provider_id VARCHAR(255),
    model_id VARCHAR(255),
    prompt_snapshot TEXT,
    archived BOOLEAN DEFAULT false NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_media_assets_project_path_fingerprint UNIQUE (project_id, physical_path, fingerprint)
);
CREATE TABLE "media_bindings" (
    id VARCHAR(36) NOT NULL,
    media_asset_id VARCHAR(36) NOT NULL,
    project_id VARCHAR(255) NOT NULL,
    binding_kind VARCHAR(32) NOT NULL,
    target_id VARCHAR(255),
    purpose VARCHAR(128) NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE,
    CONSTRAINT uq_media_bindings_semantic_reference UNIQUE (media_asset_id, project_id, binding_kind, target_id, purpose)
);
CREATE TABLE "media_derivations" (
    id VARCHAR(36) NOT NULL,
    parent_media_asset_id VARCHAR(36) NOT NULL,
    child_media_asset_id VARCHAR(36) NOT NULL,
    operation VARCHAR(32) NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(parent_media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE,
    FOREIGN KEY(child_media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE,
    CONSTRAINT uq_media_derivations_parent_child_operation UNIQUE (parent_media_asset_id, child_media_asset_id, operation)
);
CREATE TABLE "memory_candidates" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    scope VARCHAR(16) NOT NULL,
    project_name VARCHAR(200),
    category VARCHAR(32) DEFAULT 'other' NOT NULL,
    content TEXT NOT NULL,
    source VARCHAR(32) NOT NULL,
    confirmed BOOLEAN NOT NULL,
    status VARCHAR(16) DEFAULT 'pending' NOT NULL,
    source_session_id VARCHAR(128),
    source_message_id VARCHAR(128),
    metadata_json JSON,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "memory_entries" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    scope VARCHAR(16) NOT NULL,
    project_name VARCHAR(200),
    category VARCHAR(32) DEFAULT 'other' NOT NULL,
    content TEXT NOT NULL,
    source VARCHAR(32) DEFAULT 'user' NOT NULL,
    confirmed BOOLEAN DEFAULT true NOT NULL,
    source_session_id VARCHAR(128),
    source_message_id VARCHAR(128),
    metadata_json JSON,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE "project_event_log" (
    seq INTEGER NOT NULL,
    event_id VARCHAR(36) NOT NULL,
    workspace_id VARCHAR(128) NOT NULL,
    project_id VARCHAR(200) NOT NULL,
    aggregate_type VARCHAR(64) NOT NULL,
    aggregate_id VARCHAR(128) NOT NULL,
    aggregate_version INTEGER NOT NULL,
    event_type VARCHAR(128) NOT NULL,
    event_version INTEGER DEFAULT '1' NOT NULL,
    payload_json TEXT DEFAULT '{}' NOT NULL,
    causation_id VARCHAR(36),
    correlation_id VARCHAR(36),
    actor_type VARCHAR(32) DEFAULT 'user' NOT NULL,
    actor_id VARCHAR(128) NOT NULL,
    trace_id VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (seq),
    UNIQUE (event_id)
);
CREATE TABLE "provider_config" (
    id INTEGER NOT NULL,
    provider VARCHAR(32) NOT NULL,
    "key" VARCHAR(64) NOT NULL,
    value TEXT NOT NULL,
    is_secret BOOLEAN NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_provider_key UNIQUE (provider, "key")
);
CREATE TABLE "provider_credential" (
    id INTEGER NOT NULL,
    provider VARCHAR(32) NOT NULL,
    name VARCHAR(128) NOT NULL,
    api_key TEXT,
    credentials_path TEXT,
    base_url TEXT,
    is_active BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    access_key TEXT,
    secret_key TEXT,
    PRIMARY KEY (id)
);
CREATE TABLE "publishing_accounts" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    platform VARCHAR(64) NOT NULL,
    platform_account_id VARCHAR(256) NOT NULL,
    account_name VARCHAR(256) NOT NULL,
    avatar_url TEXT,
    encrypted_access_token TEXT,
    encrypted_refresh_token TEXT,
    token_expires_at TIMESTAMPTZ,
    scopes_json TEXT DEFAULT '[]' NOT NULL,
    encryption_key_version VARCHAR(16) DEFAULT 'v1' NOT NULL,
    status VARCHAR(24) DEFAULT 'active' NOT NULL,
    last_refresh_at TIMESTAMPTZ,
    last_error_code VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT uq_publishing_account_identity UNIQUE (user_id, platform, platform_account_id)
);
CREATE TABLE "render_jobs" (
    id VARCHAR(36) NOT NULL,
    plan_id VARCHAR(36) NOT NULL,
    project_name VARCHAR(200) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    revision_number INTEGER NOT NULL,
    kind VARCHAR(24) DEFAULT 'preview' NOT NULL,
    status VARCHAR(24) DEFAULT 'queued' NOT NULL,
    attempt INTEGER DEFAULT '0' NOT NULL,
    max_attempts INTEGER DEFAULT '3' NOT NULL,
    input_fingerprint VARCHAR(64) NOT NULL,
    error_code VARCHAR(64),
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    progress DOUBLE PRECISION DEFAULT '0' NOT NULL,
    progress_stage VARCHAR(32) DEFAULT 'queued' NOT NULL,
    worker_id VARCHAR(128),
    heartbeat_at TIMESTAMPTZ,
    cancel_requested_at TIMESTAMPTZ,
    PRIMARY KEY (id),
    FOREIGN KEY(plan_id) REFERENCES assembly_plans (id) ON DELETE CASCADE
);
CREATE TABLE "render_artifacts" (
    id VARCHAR(36) NOT NULL,
    render_job_id VARCHAR(36) NOT NULL,
    plan_id VARCHAR(36) NOT NULL,
    project_name VARCHAR(200) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    revision_number INTEGER NOT NULL,
    kind VARCHAR(24) DEFAULT 'preview' NOT NULL,
    relative_path VARCHAR(1000) NOT NULL,
    mime_type VARCHAR(128) DEFAULT 'video/mp4' NOT NULL,
    size_bytes INTEGER NOT NULL,
    duration_seconds DOUBLE PRECISION NOT NULL,
    width INTEGER,
    height INTEGER,
    fingerprint VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(render_job_id) REFERENCES render_jobs (id) ON DELETE CASCADE
);
CREATE TABLE "render_review_snapshots" (
    id VARCHAR(36) NOT NULL,
    artifact_id VARCHAR(36) NOT NULL,
    plan_id VARCHAR(36) NOT NULL,
    project_name VARCHAR(200) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    revision_number INTEGER NOT NULL,
    source_fingerprint VARCHAR(64) NOT NULL,
    artifact_fingerprint VARCHAR(64) NOT NULL,
    status VARCHAR(24) NOT NULL,
    checks_json TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    confirmed_by VARCHAR(128),
    confirmed_at TIMESTAMPTZ,
    PRIMARY KEY (id),
    FOREIGN KEY(artifact_id) REFERENCES render_artifacts (id) ON DELETE CASCADE
);
CREATE TABLE "publish_jobs" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    plan_id VARCHAR(36) NOT NULL,
    project_name VARCHAR(200) NOT NULL,
    artifact_id VARCHAR(36) NOT NULL,
    review_snapshot_id VARCHAR(36) NOT NULL,
    revision_number INTEGER NOT NULL,
    source_fingerprint VARCHAR(64) NOT NULL,
    artifact_fingerprint VARCHAR(64) NOT NULL,
    platform VARCHAR(64) NOT NULL,
    destination_json TEXT DEFAULT '{}' NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    request_fingerprint VARCHAR(64) NOT NULL,
    status VARCHAR(32) DEFAULT 'queued' NOT NULL,
    attempt INTEGER DEFAULT '0' NOT NULL,
    max_attempts INTEGER DEFAULT '3' NOT NULL,
    error_code VARCHAR(64),
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    account_id VARCHAR(36),
    external_content_id VARCHAR(256),
    external_status VARCHAR(64),
    next_poll_at TIMESTAMPTZ,
    last_polled_at TIMESTAMPTZ,
    worker_id VARCHAR(128),
    lease_until TIMESTAMPTZ,
    PRIMARY KEY (id),
    CONSTRAINT uq_publish_jobs_user_idempotency UNIQUE (user_id, idempotency_key),
    CONSTRAINT fk_publish_jobs_account_id FOREIGN KEY(account_id) REFERENCES publishing_accounts (id) ON DELETE SET NULL,
    FOREIGN KEY(artifact_id) REFERENCES render_artifacts (id) ON DELETE RESTRICT,
    FOREIGN KEY(plan_id) REFERENCES assembly_plans (id) ON DELETE RESTRICT,
    FOREIGN KEY(review_snapshot_id) REFERENCES render_review_snapshots (id) ON DELETE RESTRICT
);
CREATE TABLE "system_setting" (
    id INTEGER NOT NULL,
    "key" VARCHAR(64) NOT NULL,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    UNIQUE ("key")
);
CREATE TABLE "worker_lease" (
    name VARCHAR NOT NULL,
    owner_id VARCHAR NOT NULL,
    lease_until DOUBLE PRECISION NOT NULL,
    updated_at VARCHAR NOT NULL,
    PRIMARY KEY (name)
);
CREATE TABLE "workflow_approvals" (
    id VARCHAR(36) NOT NULL,
    workflow_run_id VARCHAR(36) NOT NULL,
    node_run_id VARCHAR(36),
    action VARCHAR(64) NOT NULL,
    execution_hash VARCHAR(64) NOT NULL,
    input_hash VARCHAR(64) NOT NULL,
    budget_limit DOUBLE PRECISION,
    status VARCHAR(24) DEFAULT 'pending' NOT NULL,
    requested_by VARCHAR(128) NOT NULL,
    decided_by VARCHAR(128),
    expires_at TIMESTAMPTZ,
    decided_at TIMESTAMPTZ,
    PRIMARY KEY (id),
    FOREIGN KEY(workflow_run_id) REFERENCES workflow_runs (id) ON DELETE CASCADE,
    FOREIGN KEY(node_run_id) REFERENCES workflow_node_runs (id) ON DELETE CASCADE
);
CREATE TABLE "workflow_edges" (
    id VARCHAR(36) NOT NULL,
    revision_id VARCHAR(36) NOT NULL,
    edge_key VARCHAR(128) NOT NULL,
    source_node_key VARCHAR(128) NOT NULL,
    target_node_key VARCHAR(128) NOT NULL,
    condition_json TEXT,
    on_failure VARCHAR(24) DEFAULT 'stop' NOT NULL,
    priority INTEGER DEFAULT '0' NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(revision_id) REFERENCES workflow_revisions (id) ON DELETE CASCADE,
    CONSTRAINT uq_workflow_edge_key UNIQUE (revision_id, edge_key)
);
CREATE TABLE "workflow_templates" (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR DEFAULT 'default' NOT NULL,
    name VARCHAR(200) NOT NULL,
    description TEXT DEFAULT '' NOT NULL,
    cover_ref VARCHAR(500),
    template_type VARCHAR(32) NOT NULL,
    status VARCHAR(24) DEFAULT 'draft' NOT NULL,
    draft_revision_id VARCHAR(36),
    published_revision_id VARCHAR(36),
    contract_json TEXT DEFAULT '{}' NOT NULL,
    submitted_at TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE,
    FOREIGN KEY(draft_revision_id) REFERENCES workflow_revisions (id),
    FOREIGN KEY(published_revision_id) REFERENCES workflow_revisions (id)
);
CREATE TABLE "workflow_marketplace_reviews" (
    id VARCHAR(36) NOT NULL,
    template_id VARCHAR(36) NOT NULL,
    revision_id VARCHAR(36) NOT NULL,
    status VARCHAR(24) NOT NULL,
    decision VARCHAR(32) NOT NULL,
    comment TEXT NOT NULL,
    reviewer_id VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(template_id) REFERENCES workflow_templates (id) ON DELETE CASCADE,
    FOREIGN KEY(revision_id) REFERENCES workflow_revisions (id)
);
CREATE TABLE "workflow_node_run_items" (
    id VARCHAR(36) NOT NULL,
    node_run_id VARCHAR(36) NOT NULL,
    item_key VARCHAR(200) NOT NULL,
    ordinal INTEGER NOT NULL,
    input_hash VARCHAR(64),
    input_snapshot_json TEXT,
    weight DOUBLE PRECISION DEFAULT '1' NOT NULL,
    status VARCHAR(24) DEFAULT 'blocked' NOT NULL,
    current_attempt_no INTEGER DEFAULT '1' NOT NULL,
    lineage_hash VARCHAR(64),
    output_refs_json TEXT,
    error_code VARCHAR(64),
    error_params_json TEXT,
    PRIMARY KEY (id),
    FOREIGN KEY(node_run_id) REFERENCES workflow_node_runs (id) ON DELETE CASCADE,
    CONSTRAINT uq_workflow_node_item_key UNIQUE (node_run_id, item_key)
);
CREATE TABLE "workflow_nodes" (
    id VARCHAR(36) NOT NULL,
    revision_id VARCHAR(36) NOT NULL,
    node_key VARCHAR(128) NOT NULL,
    node_type VARCHAR(128) NOT NULL,
    node_type_version VARCHAR(32) DEFAULT '1' NOT NULL,
    config_schema_version VARCHAR(32) DEFAULT '1' NOT NULL,
    config_json TEXT DEFAULT '{}' NOT NULL,
    ui_position_json TEXT,
    weight DOUBLE PRECISION DEFAULT '1' NOT NULL,
    retry_policy_json TEXT,
    approval_policy_json TEXT,
    input_schema_json TEXT DEFAULT '{}' NOT NULL,
    output_schema_json TEXT DEFAULT '{}' NOT NULL,
    executor_id VARCHAR(128) DEFAULT 'builtin' NOT NULL,
    required_capabilities_json TEXT DEFAULT '[]' NOT NULL,
    estimated_cost DOUBLE PRECISION DEFAULT '0' NOT NULL,
    cache_policy VARCHAR(32) DEFAULT 'reuse' NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_workflow_node_key UNIQUE (revision_id, node_key),
    CONSTRAINT ck_workflow_node_weight_nonnegative CHECK (weight >= 0),
    CONSTRAINT ck_workflow_node_estimated_cost_nonnegative CHECK (estimated_cost >= 0),
    FOREIGN KEY(revision_id) REFERENCES workflow_revisions (id) ON DELETE CASCADE
);
CREATE TABLE "workflow_usage_stats" (
    template_id VARCHAR(36) NOT NULL,
    views INTEGER DEFAULT '0' NOT NULL,
    derivations INTEGER DEFAULT '0' NOT NULL,
    run_count INTEGER DEFAULT '0' NOT NULL,
    successful_run_count INTEGER DEFAULT '0' NOT NULL,
    total_cost DOUBLE PRECISION DEFAULT '0' NOT NULL,
    total_duration_seconds DOUBLE PRECISION DEFAULT '0' NOT NULL,
    rating_total DOUBLE PRECISION DEFAULT '0' NOT NULL,
    rating_count INTEGER DEFAULT '0' NOT NULL,
    PRIMARY KEY (template_id),
    FOREIGN KEY(template_id) REFERENCES workflow_templates (id) ON DELETE CASCADE
);

-- 索引
CREATE INDEX ix_agent_credential_user ON agent_anthropic_credentials (user_id);
CREATE INDEX idx_agent_entries_listing ON agent_session_entries (project_key, session_id, mtime_ms);
CREATE INDEX ix_agent_session_entries_user_id ON agent_session_entries (user_id);
CREATE UNIQUE INDEX uq_agent_entries_uuid ON agent_session_entries (project_key, session_id, subpath, uuid) WHERE uuid IS NOT NULL;
CREATE INDEX ix_agent_event_log_client_key ON agent_session_event_log (client_key) WHERE client_key IS NOT NULL;
CREATE INDEX ix_agent_session_event_log_user_id ON agent_session_event_log (user_id);
CREATE UNIQUE INDEX uq_agent_event_log_client_key ON agent_session_event_log (session_id, client_key) WHERE client_key IS NOT NULL;
CREATE INDEX ix_agent_session_summaries_user_id ON agent_session_summaries (user_id);
CREATE INDEX ix_agent_session_user_message_links_user_id ON agent_session_user_message_links (user_id);
CREATE INDEX idx_agent_sessions_project ON agent_sessions (project_name, updated_at);
CREATE INDEX idx_agent_sessions_sdk_type ON agent_sessions (sdk_type);
CREATE INDEX idx_agent_sessions_status ON agent_sessions (status);
CREATE INDEX ix_agent_sessions_user_id ON agent_sessions (user_id);
CREATE INDEX idx_api_calls_call_type ON api_calls (call_type);
CREATE INDEX idx_api_calls_project_name ON api_calls (project_name);
CREATE INDEX idx_api_calls_started_at ON api_calls (started_at);
CREATE INDEX idx_api_calls_status ON api_calls (status);
CREATE INDEX idx_api_calls_task_id ON api_calls (task_id);
CREATE INDEX ix_api_calls_segment_id ON api_calls (segment_id);
CREATE INDEX ix_api_calls_user_id ON api_calls (user_id);
CREATE INDEX ix_api_keys_user_id ON api_keys (user_id);
CREATE INDEX ix_assembly_plan_revisions_plan_created ON assembly_plan_revisions (plan_id, created_at);
CREATE INDEX ix_assembly_plan_revisions_plan_id ON assembly_plan_revisions (plan_id);
CREATE INDEX ix_assembly_plans_project_name ON assembly_plans (project_name);
CREATE INDEX ix_assembly_plans_status ON assembly_plans (status);
CREATE INDEX ix_assembly_plans_user_id ON assembly_plans (user_id);
CREATE INDEX ix_assembly_plans_user_project_status ON assembly_plans (user_id, project_name, status);
CREATE INDEX ix_asset_name ON assets (name);
CREATE INDEX ix_asset_type ON assets (type);
CREATE INDEX ix_budget_reservations_workflow_run_id ON budget_reservations (workflow_run_id);
CREATE INDEX ix_budget_reservations_workspace_id ON budget_reservations (workspace_id);
CREATE INDEX ix_creation_compatibility_events_project_generation_mode ON creation_compatibility_events (project_generation_mode);
CREATE INDEX ix_creation_compatibility_events_skill_version_id ON creation_compatibility_events (creation_skill_version_id);
CREATE INDEX ix_creation_plans_project_id ON creation_plans (project_id);
CREATE INDEX ix_creation_plans_project_status ON creation_plans (project_id, status);
CREATE INDEX ix_creation_plans_user_id ON creation_plans (user_id);
CREATE INDEX ix_creation_plans_workflow_run_id ON creation_plans (workflow_run_id);
CREATE INDEX ix_creation_skill_definitions_active ON creation_skill_definitions (active);
CREATE INDEX ix_creation_skill_versions_skill_id ON creation_skill_versions (skill_id);
CREATE INDEX ix_creation_skill_versions_status ON creation_skill_versions (status);
CREATE INDEX ix_creative_board_edges_board ON creative_board_edges (board_id);
CREATE INDEX ix_creative_board_items_board ON creative_board_items (board_id);
CREATE INDEX ix_creative_board_versions_board ON creative_board_versions (board_id);
CREATE INDEX ix_creative_boards_project ON creative_boards (project_id);
CREATE INDEX ix_creative_boards_user_id ON creative_boards (user_id);
CREATE INDEX ix_custom_provider_model_provider_id ON custom_provider_model (provider_id);
CREATE INDEX idx_generation_batches_user_project_updated ON generation_batches (user_id, project_name, updated_at);
CREATE INDEX ix_generation_batches_user_id ON generation_batches (user_id);
CREATE INDEX ix_media_assets_project_kind ON media_assets (project_id, kind);
CREATE INDEX ix_media_assets_project_origin ON media_assets (project_id, origin);
CREATE INDEX ix_media_assets_project_workflow ON media_assets (project_id, workflow_run_id);
CREATE INDEX ix_media_bindings_project_target ON media_bindings (project_id, binding_kind, target_id);
CREATE INDEX ix_media_derivations_child ON media_derivations (child_media_asset_id);
CREATE INDEX ix_memory_candidates_user_id ON memory_candidates (user_id);
CREATE INDEX ix_memory_candidates_user_scope_project ON memory_candidates (user_id, scope, project_name);
CREATE INDEX ix_memory_candidates_user_status ON memory_candidates (user_id, status, updated_at);
CREATE INDEX ix_memory_entries_user_confirmed ON memory_entries (user_id, confirmed, updated_at);
CREATE INDEX ix_memory_entries_user_id ON memory_entries (user_id);
CREATE INDEX ix_memory_entries_user_scope_project ON memory_entries (user_id, scope, project_name);
CREATE INDEX ix_project_event_log_project_id ON project_event_log (project_id);
CREATE INDEX ix_project_event_log_project_seq ON project_event_log (project_id, seq);
CREATE INDEX ix_project_event_log_workspace_id ON project_event_log (workspace_id);
CREATE INDEX ix_provider_config_provider ON provider_config (provider);
CREATE INDEX ix_provider_credential_provider ON provider_credential (provider);
CREATE INDEX ix_publish_jobs_account_id ON publish_jobs (account_id);
CREATE INDEX ix_publish_jobs_artifact_id ON publish_jobs (artifact_id);
CREATE INDEX ix_publish_jobs_lease ON publish_jobs (status, lease_until);
CREATE INDEX ix_publish_jobs_plan_id ON publish_jobs (plan_id);
CREATE INDEX ix_publish_jobs_plan_revision ON publish_jobs (plan_id, revision_number);
CREATE INDEX ix_publish_jobs_poll ON publish_jobs (status, next_poll_at);
CREATE INDEX ix_publish_jobs_status ON publish_jobs (status);
CREATE INDEX ix_publish_jobs_user_id ON publish_jobs (user_id);
CREATE INDEX ix_publish_jobs_user_status ON publish_jobs (user_id, status);
CREATE INDEX ix_publishing_accounts_status ON publishing_accounts (user_id, status);
CREATE INDEX ix_publishing_accounts_user_id ON publishing_accounts (user_id);
CREATE INDEX ix_publishing_accounts_user_platform ON publishing_accounts (user_id, platform);
CREATE INDEX ix_render_artifacts_job_kind ON render_artifacts (render_job_id, kind);
CREATE INDEX ix_render_artifacts_plan_id ON render_artifacts (plan_id);
CREATE INDEX ix_render_artifacts_render_job_id ON render_artifacts (render_job_id);
CREATE INDEX ix_render_artifacts_user_id ON render_artifacts (user_id);
CREATE INDEX ix_render_artifacts_user_project ON render_artifacts (user_id, project_name);
CREATE INDEX ix_render_jobs_heartbeat ON render_jobs (status, heartbeat_at);
CREATE INDEX ix_render_jobs_plan_id ON render_jobs (plan_id);
CREATE INDEX ix_render_jobs_plan_revision ON render_jobs (plan_id, revision_number);
CREATE INDEX ix_render_jobs_status ON render_jobs (status);
CREATE INDEX ix_render_jobs_user_id ON render_jobs (user_id);
CREATE INDEX ix_render_jobs_user_project_status ON render_jobs (user_id, project_name, status);
CREATE UNIQUE INDEX uq_render_jobs_active_plan_revision_kind ON render_jobs (plan_id, revision_number, kind) WHERE status IN ('queued', 'running', 'cancelling');
CREATE INDEX ix_render_review_snapshots_artifact_created ON render_review_snapshots (artifact_id, created_at);
CREATE INDEX ix_render_review_snapshots_artifact_id ON render_review_snapshots (artifact_id);
CREATE INDEX ix_render_review_snapshots_plan_id ON render_review_snapshots (plan_id);
CREATE INDEX ix_render_review_snapshots_user_id ON render_review_snapshots (user_id);
CREATE INDEX ix_render_review_snapshots_user_project ON render_review_snapshots (user_id, project_name);
CREATE UNIQUE INDEX idx_tasks_dedupe_active ON tasks (project_name, task_type, resource_id, COALESCE(script_file, ''), COALESCE(resource_type, '')) WHERE status IN ('queued', 'running', 'cancelling');
CREATE INDEX idx_tasks_dependency_task_id ON tasks (dependency_task_id);
CREATE INDEX idx_tasks_project_updated_at ON tasks (project_name, updated_at);
CREATE INDEX idx_tasks_status_provider_queued ON tasks (status, provider_id, queued_at);
CREATE INDEX idx_tasks_status_queued_at ON tasks (status, queued_at);
CREATE INDEX ix_tasks_fencing_token ON tasks (fencing_token);
CREATE INDEX ix_tasks_input_fingerprint ON tasks (input_fingerprint);
CREATE INDEX ix_tasks_lease_owner ON tasks (lease_owner);
CREATE INDEX ix_tasks_lease_until ON tasks (lease_until);
CREATE INDEX ix_tasks_phase_code ON tasks (phase_code);
CREATE INDEX ix_tasks_progress ON tasks (progress);
CREATE INDEX ix_tasks_progress_source ON tasks (progress_source);
CREATE INDEX ix_tasks_request_idempotency_key ON tasks (request_idempotency_key);
CREATE INDEX ix_tasks_user_id ON tasks (user_id);
CREATE INDEX ix_tasks_workflow_node_run_id ON tasks (workflow_node_run_id);
CREATE INDEX ix_tasks_workflow_node_run_item_id ON tasks (workflow_node_run_item_id);
CREATE INDEX ix_tasks_workflow_run_id ON tasks (workflow_run_id);
CREATE INDEX ix_workflow_approvals_workflow_run_id ON workflow_approvals (workflow_run_id);
CREATE INDEX ix_workflow_definitions_project_id ON workflow_definitions (project_id);
CREATE INDEX ix_workflow_definitions_user_id ON workflow_definitions (user_id);
CREATE INDEX ix_workflow_definitions_workspace_id ON workflow_definitions (workspace_id);
CREATE INDEX ix_workflow_edges_revision_id ON workflow_edges (revision_id);
CREATE INDEX ix_workflow_marketplace_reviews_template_created ON workflow_marketplace_reviews (template_id, created_at);
CREATE INDEX ix_workflow_marketplace_reviews_template_id ON workflow_marketplace_reviews (template_id);
CREATE INDEX ix_workflow_node_run_items_node_run_id ON workflow_node_run_items (node_run_id);
CREATE INDEX ix_workflow_node_runs_workflow_run_id ON workflow_node_runs (workflow_run_id);
CREATE INDEX ix_workflow_nodes_revision_id ON workflow_nodes (revision_id);
CREATE INDEX ix_workflow_revisions_definition_id ON workflow_revisions (definition_id);
CREATE INDEX ix_workflow_runs_episode_id ON workflow_runs (episode_id);
CREATE INDEX ix_workflow_runs_input_fingerprint ON workflow_runs (input_fingerprint);
CREATE INDEX ix_workflow_runs_project_id ON workflow_runs (project_id);
CREATE INDEX ix_workflow_runs_project_status ON workflow_runs (project_id, status);
CREATE INDEX ix_workflow_runs_trace_id ON workflow_runs (trace_id);
CREATE INDEX ix_workflow_runs_user_id ON workflow_runs (user_id);
CREATE INDEX ix_workflow_runs_workflow_revision_id ON workflow_runs (workflow_revision_id);
CREATE INDEX ix_workflow_runs_workspace_id ON workflow_runs (workspace_id);
CREATE INDEX ix_workflow_templates_status ON workflow_templates (status);
CREATE INDEX ix_workflow_templates_template_type ON workflow_templates (template_type);
CREATE INDEX ix_workflow_templates_user_id ON workflow_templates (user_id);

-- +migrate Down
DROP TABLE IF EXISTS "workflow_usage_stats";
DROP TABLE IF EXISTS "workflow_nodes";
DROP TABLE IF EXISTS "workflow_node_run_items";
DROP TABLE IF EXISTS "workflow_marketplace_reviews";
DROP TABLE IF EXISTS "workflow_templates";
DROP TABLE IF EXISTS "workflow_edges";
DROP TABLE IF EXISTS "workflow_approvals";
DROP TABLE IF EXISTS "worker_lease";
DROP TABLE IF EXISTS "system_setting";
DROP TABLE IF EXISTS "publish_jobs";
DROP TABLE IF EXISTS "render_review_snapshots";
DROP TABLE IF EXISTS "render_artifacts";
DROP TABLE IF EXISTS "render_jobs";
DROP TABLE IF EXISTS "publishing_accounts";
DROP TABLE IF EXISTS "provider_credential";
DROP TABLE IF EXISTS "provider_config";
DROP TABLE IF EXISTS "project_event_log";
DROP TABLE IF EXISTS "memory_entries";
DROP TABLE IF EXISTS "memory_candidates";
DROP TABLE IF EXISTS "media_derivations";
DROP TABLE IF EXISTS "media_bindings";
DROP TABLE IF EXISTS "media_assets";
DROP TABLE IF EXISTS "generation_batches";
DROP TABLE IF EXISTS "external_executions";
DROP TABLE IF EXISTS "tasks";
DROP TABLE IF EXISTS "custom_provider_model";
DROP TABLE IF EXISTS "custom_provider";
DROP TABLE IF EXISTS "creative_board_versions";
DROP TABLE IF EXISTS "creative_board_edges";
DROP TABLE IF EXISTS "creative_board_items";
DROP TABLE IF EXISTS "creative_boards";
DROP TABLE IF EXISTS "creation_skill_compatibilities";
DROP TABLE IF EXISTS "creation_skill_versions";
DROP TABLE IF EXISTS "creation_skill_definitions";
DROP TABLE IF EXISTS "creation_plans";
DROP TABLE IF EXISTS "creation_compatibility_events";
DROP TABLE IF EXISTS "budget_reservations";
DROP TABLE IF EXISTS "workflow_node_runs";
DROP TABLE IF EXISTS "workflow_runs";
DROP TABLE IF EXISTS "workflow_revisions";
DROP TABLE IF EXISTS "workflow_definitions";
DROP TABLE IF EXISTS "assets";
DROP TABLE IF EXISTS "assembly_plan_revisions";
DROP TABLE IF EXISTS "assembly_plans";
DROP TABLE IF EXISTS "api_keys";
DROP TABLE IF EXISTS "api_calls";
DROP TABLE IF EXISTS "agent_sessions";
DROP TABLE IF EXISTS "agent_session_user_message_links";
DROP TABLE IF EXISTS "agent_session_summaries";
DROP TABLE IF EXISTS "agent_session_event_log";
DROP TABLE IF EXISTS "agent_session_entries";
DROP TABLE IF EXISTS "users";
DROP TABLE IF EXISTS "agent_anthropic_credentials";
