-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS menu_states (
    user_id      VARCHAR(255) PRIMARY KEY,
    state_type   VARCHAR(32)  NOT NULL,
    current_node VARCHAR(64),
    scene_id     VARCHAR(64),
    expires_at   TIMESTAMP,
    updated_at   TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_menu_states_expires ON menu_states(expires_at) WHERE state_type = 'pending_freetext' AND expires_at IS NOT NULL;

COMMENT ON TABLE menu_states IS 'wecom-bot command-mode user state (menu_node for command_text, pending_freetext for command_card)';
COMMENT ON COLUMN menu_states.state_type IS 'menu_node (no TTL) | pending_freetext (5min TTL)';
COMMENT ON COLUMN menu_states.current_node IS 'current menu node id, e.g. root / task.list (state_type=menu_node only)';
COMMENT ON COLUMN menu_states.scene_id IS 'pending free-text scene, e.g. ques:open012 (state_type=pending_freetext only)';
COMMENT ON COLUMN menu_states.expires_at IS 'TTL for pending_freetext; NULL for menu_node';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS menu_states;

-- +goose StatementEnd
