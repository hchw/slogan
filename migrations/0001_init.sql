-- 0001_init.sql : initial schema for the multi-model gateway.
-- Money is stored as integer micro-yuan (1e-6 yuan). Times are timestamptz (UTC).

-- ---------------------------------------------------------------- identity
CREATE TABLE admin_user (
    id            BIGSERIAL PRIMARY KEY,
    username      VARCHAR(64)  NOT NULL,
    display_name  VARCHAR(128) NOT NULL DEFAULT '',
    password_hash VARCHAR(255) NOT NULL,
    status        VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_admin_user_username ON admin_user(username) WHERE deleted_at IS NULL;

CREATE TABLE role (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(64)  NOT NULL,
    name        VARCHAR(128) NOT NULL,
    permissions JSONB        NOT NULL DEFAULT '[]'::jsonb,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_role_code ON role(code) WHERE deleted_at IS NULL;

CREATE TABLE admin_user_role (
    id            BIGSERIAL PRIMARY KEY,
    admin_user_id BIGINT NOT NULL REFERENCES admin_user(id) ON DELETE CASCADE,
    role_id       BIGINT NOT NULL REFERENCES role(id) ON DELETE CASCADE,
    UNIQUE (admin_user_id, role_id)
);

CREATE TABLE app_user (
    id            BIGSERIAL PRIMARY KEY,
    email         VARCHAR(191) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    nickname      VARCHAR(128) NOT NULL DEFAULT '',
    status        VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_app_user_email ON app_user(email) WHERE deleted_at IS NULL;

-- Revocable opaque sessions for users and administrators.
CREATE TABLE session (
    id             BIGSERIAL PRIMARY KEY,
    principal_type VARCHAR(16) NOT NULL, -- user | admin
    principal_id   BIGINT      NOT NULL,
    token_hash     VARCHAR(128) NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_session_token ON session(token_hash);
CREATE INDEX idx_session_principal ON session(principal_type, principal_id);

CREATE TABLE api_key (
    id                 BIGSERIAL PRIMARY KEY,
    user_id            BIGINT       NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    name               VARCHAR(64)  NOT NULL DEFAULT 'default',
    key_prefix         VARCHAR(16)  NOT NULL,
    key_hash           VARCHAR(128) NOT NULL,
    status             VARCHAR(16)  NOT NULL DEFAULT 'active',
    expires_at         TIMESTAMPTZ,
    allowed_models     JSONB,
    rate_limit_per_min INTEGER,
    last_used_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at         TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_api_key_hash ON api_key(key_hash) WHERE deleted_at IS NULL;
CREATE INDEX idx_api_key_user ON api_key(user_id);

-- ---------------------------------------------------------------- catalog
CREATE TABLE provider (
    id            BIGSERIAL PRIMARY KEY,
    name          VARCHAR(128) NOT NULL,
    type          VARCHAR(32)  NOT NULL DEFAULT 'openai_compatible',
    base_url      VARCHAR(512) NOT NULL,
    auth_type     VARCHAR(32)  NOT NULL DEFAULT 'bearer',
    secret_cipher TEXT,
    extension_allowlist JSONB NOT NULL DEFAULT '[]'::jsonb,
    protocol      VARCHAR(32)  NOT NULL DEFAULT 'openai',
    region        VARCHAR(64)  NOT NULL DEFAULT '',
    status        VARCHAR(16)  NOT NULL DEFAULT 'enabled',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);

CREATE TABLE model (
    id                  BIGSERIAL PRIMARY KEY,
    provider_id         BIGINT       NOT NULL REFERENCES provider(id) ON DELETE CASCADE,
    name                VARCHAR(128) NOT NULL,
    model_key           VARCHAR(128) NOT NULL,
    context_length      INTEGER,
    input_modalities    JSONB        NOT NULL DEFAULT '["text"]'::jsonb,
    supports_stream     BOOLEAN      NOT NULL DEFAULT true,
    supports_tools      BOOLEAN      NOT NULL DEFAULT false,
    supports_vision     BOOLEAN      NOT NULL DEFAULT false,
    input_price_micro   BIGINT       NOT NULL DEFAULT 0, -- upstream cost / input token
    output_price_micro  BIGINT       NOT NULL DEFAULT 0, -- upstream cost / output token
    charge_input_micro  BIGINT       NOT NULL DEFAULT 0, -- user charge / input token
    charge_output_micro BIGINT       NOT NULL DEFAULT 0, -- user charge / output token
    price_version       VARCHAR(32)  NOT NULL DEFAULT 'v1',
    source              VARCHAR(16)  NOT NULL DEFAULT 'manual',
    status              VARCHAR(16)  NOT NULL DEFAULT 'available',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_model_provider_key ON model(provider_id, model_key) WHERE deleted_at IS NULL;
CREATE INDEX idx_model_status ON model(status);

-- ---------------------------------------------------------------- evaluation
CREATE TABLE score_version (
    id               BIGSERIAL PRIMARY KEY,
    status           VARCHAR(16)  NOT NULL DEFAULT 'building',
    success_ratio    NUMERIC(5,4) NOT NULL DEFAULT 0,
    total_models     INTEGER      NOT NULL DEFAULT 0,
    valid_models     INTEGER      NOT NULL DEFAULT 0,
    template_version VARCHAR(32)  NOT NULL DEFAULT 'v1',
    rule_version     VARCHAR(32)  NOT NULL DEFAULT 'v1',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_score_version_published ON score_version((status)) WHERE status = 'published';

CREATE TABLE model_capability (
    id               BIGSERIAL PRIMARY KEY,
    score_version_id BIGINT       NOT NULL REFERENCES score_version(id) ON DELETE CASCADE,
    model_id         BIGINT       NOT NULL REFERENCES model(id) ON DELETE CASCADE,
    dimension        VARCHAR(32)  NOT NULL,
    score            NUMERIC(5,4) NOT NULL DEFAULT 0,
    self_score       NUMERIC(5,4) NOT NULL DEFAULT 0,
    peer_score       NUMERIC(5,4) NOT NULL DEFAULT 0,
    confidence       NUMERIC(5,4) NOT NULL DEFAULT 0,
    UNIQUE (score_version_id, model_id, dimension)
);

CREATE TABLE model_evaluation (
    id                    BIGSERIAL PRIMARY KEY,
    refresh_task_id       BIGINT,
    evaluator_model_id    BIGINT       NOT NULL REFERENCES model(id) ON DELETE CASCADE,
    target_model_id       BIGINT       NOT NULL REFERENCES model(id) ON DELETE CASCADE,
    dimension             VARCHAR(32)  NOT NULL,
    score                 NUMERIC(5,4) NOT NULL DEFAULT 0,
    confidence            NUMERIC(5,4) NOT NULL DEFAULT 0,
    weight                NUMERIC(5,4) NOT NULL DEFAULT 0,
    output_tokens         INTEGER      NOT NULL DEFAULT 0,
    raw_output            JSONB,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX idx_eval_task ON model_evaluation(refresh_task_id);

CREATE TABLE refresh_task (
    id               BIGSERIAL PRIMARY KEY,
    score_version_id BIGINT REFERENCES score_version(id),
    status           VARCHAR(16) NOT NULL DEFAULT 'running',
    total            INTEGER     NOT NULL DEFAULT 0,
    succeeded        INTEGER     NOT NULL DEFAULT 0,
    failed           INTEGER     NOT NULL DEFAULT 0,
    cost_micro       BIGINT      NOT NULL DEFAULT 0,
    frozen_participants JSONB,
    created_by       BIGINT REFERENCES admin_user(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ
);

CREATE TABLE refresh_task_item (
    id            BIGSERIAL PRIMARY KEY,
    task_id       BIGINT      NOT NULL REFERENCES refresh_task(id) ON DELETE CASCADE,
    model_id      BIGINT      NOT NULL REFERENCES model(id) ON DELETE CASCADE,
    status        VARCHAR(24) NOT NULL DEFAULT 'pending',
    error_message TEXT,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micro   BIGINT NOT NULL DEFAULT 0,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);
CREATE INDEX idx_refresh_item_task ON refresh_task_item(task_id);

-- ---------------------------------------------------------------- runtime
CREATE TABLE route_decision (
    id                BIGSERIAL PRIMARY KEY,
    request_id        VARCHAR(64) NOT NULL,
    intent            VARCHAR(64),
    selected_model_id BIGINT,
    score_version_id  BIGINT,
    policy_snapshot   JSONB,
    candidates        JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_route_request ON route_decision(request_id);

CREATE TABLE request_record (
    id                     BIGSERIAL PRIMARY KEY,
    request_id             VARCHAR(64) NOT NULL UNIQUE,
    user_id                BIGINT,
    api_key_id             BIGINT,
    requested_model        VARCHAR(64),
    model_id               BIGINT,
    status                 VARCHAR(24) NOT NULL,
    error_code             VARCHAR(64),
    idempotency_key        VARCHAR(128),
    request_digest         VARCHAR(64),
    intent                 VARCHAR(64),
    score_version_id       BIGINT,
    policy_version         VARCHAR(32),
    classifier_version     VARCHAR(128),
    cost_input_micro       BIGINT NOT NULL DEFAULT 0,
    cost_output_micro      BIGINT NOT NULL DEFAULT 0,
    charge_input_micro     BIGINT NOT NULL DEFAULT 0,
    charge_output_micro    BIGINT NOT NULL DEFAULT 0,
    estimated_input_tokens INTEGER NOT NULL DEFAULT 0,
    reserved_micro         BIGINT NOT NULL DEFAULT 0,
    usage_source           VARCHAR(24),
    input_tokens           INTEGER NOT NULL DEFAULT 0,
    output_tokens          INTEGER NOT NULL DEFAULT 0,
    cost_micro             BIGINT NOT NULL DEFAULT 0,
    charge_micro           BIGINT NOT NULL DEFAULT 0,
    latency_ms             INTEGER NOT NULL DEFAULT 0,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at            TIMESTAMPTZ
);
CREATE INDEX idx_req_user_time ON request_record(user_id, created_at);
CREATE INDEX idx_req_model_time ON request_record(model_id, created_at);
CREATE UNIQUE INDEX uq_request_idem ON request_record(user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE usage_event (
    request_id    VARCHAR(64) PRIMARY KEY,
    user_id       BIGINT NOT NULL,
    model_id      BIGINT NOT NULL,
    provider_id   BIGINT NOT NULL,
    input_tokens  BIGINT NOT NULL,
    output_tokens BIGINT NOT NULL,
    cost_micro    BIGINT NOT NULL,
    charge_micro  BIGINT NOT NULL,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE usage_record (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL,
    model_id      BIGINT NOT NULL,
    provider_id   BIGINT NOT NULL,
    request_count BIGINT NOT NULL DEFAULT 0,
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cost_micro    BIGINT NOT NULL DEFAULT 0,
    charge_micro  BIGINT NOT NULL DEFAULT 0,
    stat_date     DATE     NOT NULL,
    stat_hour     SMALLINT NOT NULL DEFAULT 0,
    UNIQUE (user_id, model_id, stat_date, stat_hour)
);

-- ---------------------------------------------------------------- billing
CREATE TABLE quota_account (
    id             BIGSERIAL PRIMARY KEY,
    user_id        BIGINT NOT NULL UNIQUE REFERENCES app_user(id) ON DELETE CASCADE,
    balance_micro  BIGINT NOT NULL DEFAULT 0,
    reserved_micro BIGINT NOT NULL DEFAULT 0,
    version        BIGINT NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_balance_nonneg CHECK (balance_micro >= 0),
    CONSTRAINT chk_reserved_nonneg CHECK (reserved_micro >= 0)
);

CREATE TABLE quota_ledger (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT       NOT NULL,
    type                VARCHAR(24)  NOT NULL,
    amount_micro        BIGINT       NOT NULL,
    balance_after_micro BIGINT       NOT NULL,
    ref_type            VARCHAR(24),
    ref_id              VARCHAR(64),
    remark              VARCHAR(255) NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX idx_ledger_user_time ON quota_ledger(user_id, created_at);
CREATE UNIQUE INDEX uq_ledger_reserve_ref ON quota_ledger(ref_type, ref_id) WHERE type = 'reserve';
CREATE UNIQUE INDEX uq_ledger_settle_ref ON quota_ledger(ref_type, ref_id) WHERE type IN ('consume', 'release');
CREATE UNIQUE INDEX uq_ledger_grant_ref ON quota_ledger(ref_type, ref_id) WHERE type = 'grant';
CREATE UNIQUE INDEX uq_ledger_adjust_ref ON quota_ledger(ref_type, ref_id) WHERE type = 'adjust';

CREATE TABLE quota_package (
    id               BIGSERIAL PRIMARY KEY,
    name             VARCHAR(128) NOT NULL,
    face_value_micro BIGINT       NOT NULL,
    -- Reserved for a later release; ignored in the first release.
    valid_days       INTEGER,
    model_scope      JSONB,
    allow_auto_route BOOLEAN      NOT NULL DEFAULT true,
    status           VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at       TIMESTAMPTZ
);

CREATE TABLE redemption_code_batch (
    id         BIGSERIAL PRIMARY KEY,
    package_id BIGINT NOT NULL REFERENCES quota_package(id),
    quantity   INTEGER NOT NULL,
    created_by BIGINT REFERENCES admin_user(id),
    remark     VARCHAR(255) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE TABLE redemption_code (
    id          BIGSERIAL PRIMARY KEY,
    batch_id    BIGINT      NOT NULL REFERENCES redemption_code_batch(id) ON DELETE CASCADE,
    package_id  BIGINT      NOT NULL REFERENCES quota_package(id),
    code_hash   VARCHAR(64) NOT NULL,
    code_prefix VARCHAR(16) NOT NULL,
    status      VARCHAR(16) NOT NULL DEFAULT 'unused',
    redeemed_by BIGINT REFERENCES app_user(id),
    redeemed_at TIMESTAMPTZ,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_redemption_code_hash ON redemption_code(code_hash) WHERE deleted_at IS NULL;
CREATE INDEX idx_code_status ON redemption_code(status);

-- ---------------------------------------------------------------- policy/audit
CREATE TABLE routing_policy (
    id                       BIGSERIAL PRIMARY KEY,
    low_capability_bias      INTEGER      NOT NULL DEFAULT 50,
    min_publish_ratio        NUMERIC(5,4) NOT NULL DEFAULT 0.80,
    high_risk_force_quality  BOOLEAN      NOT NULL DEFAULT true,
    eval_max_tokens          INTEGER      NOT NULL DEFAULT 2000,
    eval_concurrency         INTEGER      NOT NULL DEFAULT 5,
    eval_timeout_seconds     INTEGER      NOT NULL DEFAULT 60,
    content_log_enabled      BOOLEAN      NOT NULL DEFAULT false,
    content_log_retention_days INTEGER    NOT NULL DEFAULT 7,
    version                  VARCHAR(32)  NOT NULL DEFAULT 'v1',
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
    id          BIGSERIAL PRIMARY KEY,
    actor_type  VARCHAR(16) NOT NULL,
    actor_id    BIGINT,
    action      VARCHAR(64) NOT NULL,
    target_type VARCHAR(32),
    target_id   VARCHAR(64),
    reason      VARCHAR(255) NOT NULL DEFAULT '',
    result      VARCHAR(16) NOT NULL DEFAULT 'success',
    request_id  VARCHAR(64),
    detail      JSONB,
    ip          VARCHAR(64) NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_actor_time ON audit_log(actor_type, actor_id, created_at);
CREATE INDEX idx_audit_action_time ON audit_log(action, created_at);
