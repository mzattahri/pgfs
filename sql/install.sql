-- Creates the schema and metadata table used by pgfs.

CREATE SCHEMA IF NOT EXISTS pgfs;

CREATE TABLE IF NOT EXISTS pgfs.metadata (
	id UUID NOT NULL PRIMARY KEY,
	oid OID NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
	sys JSONB,
	content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
	content_size BIGINT NOT NULL,
	content_sha256 BYTEA NOT NULL
);
