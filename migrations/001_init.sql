CREATE TABLE uploads (
    id TEXT PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    tenant_id TEXT NOT NULL,
    filename TEXT NOT NULL,
    size BIGINT NOT NULL CHECK (size > 0),
    chunk_size BIGINT NOT NULL CHECK (chunk_size BETWEEN 65536 AND 16777216),
    total_parts INTEGER NOT NULL CHECK (total_parts BETWEEN 1 AND 10000),
    sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL DEFAULT 'UPLOADING'
        CHECK (status IN ('UPLOADING', 'COMPLETED', 'CANCELED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    CHECK ((status = 'COMPLETED') = (completed_at IS NOT NULL))
);

CREATE INDEX uploads_tenant_created ON uploads (tenant_id, created_at DESC);

CREATE TABLE upload_chunks (
    upload_id TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
    part_number INTEGER NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    size BIGINT NOT NULL CHECK (size > 0),
    sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (upload_id, part_number)
);
