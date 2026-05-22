-- Create pgvector extension as superuser
CREATE EXTENSION IF NOT EXISTS vector;

-- Grant permissions to costrict user
GRANT ALL PRIVILEGES ON DATABASE costrict_db TO costrict;
