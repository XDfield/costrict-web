-- +goose Up
-- +goose StatementBegin

INSERT INTO resource_permissions (resource_code, resource_type, allowed_roles) VALUES
    ('collaboration', 'menu', '{}')
ON CONFLICT (resource_code) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM resource_permissions WHERE resource_code = 'collaboration';

-- +goose StatementEnd
