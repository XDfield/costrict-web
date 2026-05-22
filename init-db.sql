-- Create pgvector extension as superuser
CREATE EXTENSION IF NOT EXISTS vector;

-- Grant permissions to costrict user
GRANT ALL PRIVILEGES ON DATABASE costrict_db TO costrict;

-- Casdoor: separate user + database (casdoor auto-creates its tables on first boot)
CREATE USER casdoor WITH PASSWORD 'casdoor_password';
CREATE DATABASE casdoor OWNER casdoor;
GRANT ALL PRIVILEGES ON DATABASE casdoor TO casdoor;
