package controller

// SQLite schema (ADR-0004: Postgres is truth in prod; SQLite serves
// single-node dev). Spec and enum-carrying columns are JSON text so the
// shape stays portable to Postgres (Task 4 reuses it); filter-facing
// columns (audit_events, usage_samples, local auth) are plain columns so
// SQL WHERE clauses can do the filtering instead of an application-side
// scan.
//
// Ported from mobula-controller/src/store_sqlite.rs's `SCHEMA` const
// (store_sqlite.rs:23-156). The Rust reference also carries a
// `COLUMN_MIGRATIONS` slice of idempotent `ALTER TABLE ... ADD COLUMN`
// statements (store_sqlite.rs:162-175) plus a one-time chain-hash backfill
// pass (store_sqlite.rs:191-220): both exist there ONLY to upgrade
// databases created by an *older* Rust schema before those columns/the
// audit chain existed. Bifrost's SQLite schema has no such prior version —
// every Go binary that has ever run against a Bifrost-created database
// created it with this exact shape — so there is nothing to migrate from,
// and this file has no ALTER-TABLE-if-missing step or backfill pass at
// all (ADR-0004's ruling: no production legacy chains exist, so the chain
// is implemented fresh). Additive changes after that first shape go into
// sqliteColumnMigrations below: the CREATE TABLE here carries the current
// column set for fresh databases, and the migration adds the column to a
// database created before it existed.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS clusters (
    id                  TEXT PRIMARY KEY,
    spec_json           TEXT NOT NULL,
    generation          INTEGER NOT NULL,
    desired             TEXT NOT NULL,
    observed_state      TEXT,
    observed_generation INTEGER NOT NULL DEFAULT 0,
    condition           TEXT,
    failure_count       INTEGER NOT NULL DEFAULT 0,
    next_attempt_at     INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL DEFAULT 0,
    terminated_at       INTEGER
);
CREATE TABLE IF NOT EXISTS intents (
    intent_key         TEXT PRIMARY KEY,
    params_fingerprint TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'applied',
    response_json      TEXT,
    created_at         INTEGER NOT NULL DEFAULT 0,
    completed_at       INTEGER
);
-- Singleton control flags: quarantine (#41) and the governance policy row.
CREATE TABLE IF NOT EXISTS control (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- Persistent job history. Deliberately no foreign key to clusters: records
-- outlive the clusters that ran them.
CREATE TABLE IF NOT EXISTS jobs (
    id            TEXT PRIMARY KEY,
    cluster       TEXT NOT NULL,
    submitter     TEXT NOT NULL,
    status        TEXT NOT NULL,
    duration_secs INTEGER,
    submitted_at  INTEGER NOT NULL
);
-- Capacity pools (ADR-0010): the store is truth; Kueue objects are
-- actuation. observed_json holds the pool reconcile loop's last
-- ClusterQueue status observation (opaque JSON).
CREATE TABLE IF NOT EXISTS pools (
    name          TEXT PRIMARY KEY,
    spec_json     TEXT NOT NULL,
    generation    INTEGER NOT NULL,
    observed_json TEXT,
    observed_at   INTEGER,
    created_at    INTEGER NOT NULL DEFAULT 0
);
-- Per-project allocations within a pool, keyed by (pool, project).
CREATE TABLE IF NOT EXISTS allocations (
    pool      TEXT NOT NULL,
    project   TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    PRIMARY KEY (pool, project)
);
-- Usage metering timeseries: append-only, no primary key. project = '' is
-- the pool-level aggregate row; pool = '' means the project has no
-- allocation. source is 'kueue_ledger' or 'observed_spec'.
-- owner is the per-user attribution (requirement 14); '' = unattributed.
CREATE TABLE IF NOT EXISTS usage_samples (
    ts       INTEGER NOT NULL,
    project  TEXT NOT NULL,
    pool     TEXT NOT NULL,
    resource TEXT NOT NULL,
    quantity REAL NOT NULL,
    source   TEXT NOT NULL,
    owner    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS usage_samples_project_ts ON usage_samples (project, ts);
-- Serve services (requirements 1/2): the store is truth for desired state;
-- the RayService is actuation. Same column shape as clusters minus the
-- backoff/drift columns the service reconciler does not need yet.
CREATE TABLE IF NOT EXISTS services (
    name           TEXT PRIMARY KEY,
    spec_json      TEXT NOT NULL,
    owner          TEXT,
    generation     INTEGER NOT NULL,
    desired        TEXT NOT NULL,
    observed_state TEXT,
    observed_url   TEXT,
    created_at     INTEGER NOT NULL DEFAULT 0,
    terminated_at  INTEGER
);
-- Ephemeral Ray jobs (requirement 5): submitted intent plus the last
-- KubeRay RayJob observation. No generation: a job spec is submitted once.
CREATE TABLE IF NOT EXISTS ray_jobs (
    id                TEXT PRIMARY KEY,
    spec_json         TEXT NOT NULL,
    owner             TEXT,
    desired           TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT '',
    deployment_status TEXT NOT NULL DEFAULT '',
    cluster_name      TEXT,
    dashboard_url     TEXT,
    message           TEXT,
    submitted_at      INTEGER NOT NULL DEFAULT 0,
    started_at        INTEGER,
    finished_at       INTEGER,
    failure_count     INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   INTEGER NOT NULL DEFAULT 0
);
-- Persisted audit trail (api-v1.md §5.9): append-only. seq is the
-- pagination cursor (rows are read newest-first). chain_hash is the
-- tamper-evidence chain: sha256 over (previous row's chain_hash ‖ this
-- row's canonical JSON); genesis chains from 64 zeros (AuditGenesisHash).
-- Deliberately NOT NULL with no DEFAULT '', unlike the Rust reference's
-- chain_hash TEXT NOT NULL DEFAULT '': that default existed there only
-- so the pre-chain backfill pass (rows written before the column existed)
-- had a sentinel to find and fill in. No such backfill exists here (ADR-
-- 0004: no legacy chains), so every row this schema ever writes carries a
-- real hash from RecordAudit — a row that somehow lacked one should fail
-- the INSERT loudly (a NOT NULL constraint violation), not silently
-- accept an empty-string chain_hash that VerifyAuditChain would then have
-- to special-case as "unchained" rather than treat as a genuine break.
CREATE TABLE IF NOT EXISTS audit_events (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    subject       TEXT,
    decision      TEXT NOT NULL,
    reason        TEXT,
    action        TEXT,
    cluster       TEXT,
    method        TEXT,
    path          TEXT,
    status        INTEGER,
    latency_ms    INTEGER,
    required_json TEXT,
    granted_roles TEXT NOT NULL DEFAULT '[]',
    chain_hash    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_events_ts ON audit_events (ts);
-- Local auth (ADR-0011): users and opaque API tokens. Bifrost stores
-- credentials, never signs them — both secret columns hold bcrypt hashes.
CREATE TABLE IF NOT EXISTS local_users (
    username      TEXT PRIMARY KEY,
    email         TEXT,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL DEFAULT 0,
    failed_logins INTEGER NOT NULL DEFAULT 0,
    locked_until  INTEGER
);
-- Opaque API tokens (bfr_<prefix>_<32 hex>). The 8-char prefix is the
-- lookup key; the plaintext token is never stored.
CREATE TABLE IF NOT EXISTS api_tokens (
    prefix       TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL,
    username     TEXT NOT NULL,
    label        TEXT NOT NULL,
    created_at   INTEGER NOT NULL DEFAULT 0,
    expires_at   INTEGER NOT NULL,
    revoked      INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER
);
CREATE INDEX IF NOT EXISTS api_tokens_username ON api_tokens (username);
-- Scoped role assignments: scope is '*' (global) or 'project:<name>'; role
-- is the auth package's Role wire name. Keyed by the full triple so
-- re-upsert replaces; the (principal) prefix serves the per-request authz
-- lookup.
CREATE TABLE IF NOT EXISTS role_assignments (
    principal  TEXT NOT NULL,
    role       TEXT NOT NULL,
    scope      TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (principal, role, scope)
);
`

// sqliteColumnMigration is one idempotent additive column migration: when
// Table exists without Column, Statement is run. SQLite has no `ADD COLUMN
// IF NOT EXISTS`, so NewSqliteStore checks PRAGMA table_info first.
type sqliteColumnMigration struct {
	Table, Column, Statement string
}

// sqliteColumnMigrations upgrades databases created by an older Bifrost
// schema. Append-only; every entry must also be reflected in sqliteSchema
// so a fresh database never needs it.
var sqliteColumnMigrations = []sqliteColumnMigration{
	// Requirement 14 per-user attribution: samples recorded before the
	// column existed are unattributed ('').
	{Table: "usage_samples", Column: "owner",
		Statement: "ALTER TABLE usage_samples ADD COLUMN owner TEXT NOT NULL DEFAULT ''"},
}
