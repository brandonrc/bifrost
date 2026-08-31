package controller

// Shared domain-error constructors for the Store interface's business-rule
// failures (missing entity, duplicate key, bad enum value on read). Every
// backend — MemoryStore, SqliteStore, and Task 4's forthcoming
// PostgresStore — must produce byte-identical wording for these, because
// storetest's conformance suite asserts on several of them verbatim (e.g.
// "no such pool ghost", "no such allocation gpu/proj-a"). Centralizing the
// format string here, instead of each backend duplicating its own
// storeErrorf call, makes that match-by-construction rather than a
// copy-paste discipline three backends have to independently uphold.
//
// core.ClusterId is intentionally not imported here: every call site below
// already has it as a string (id.String()/string(id)) or a plain string
// key, so these helpers stay untyped and reusable from any backend package
// without adding an import.

func errBadDesiredState(s string) error {
	return storeErrorf("bad desired state %q", s)
}

func errNoSuchCluster(id string) error {
	return storeErrorf("no such cluster %s", id)
}

func errNoSuchPool(name string) error {
	return storeErrorf("no such pool %s", name)
}

func errNoSuchAllocation(pool, project string) error {
	return storeErrorf("no such allocation %s/%s", pool, project)
}

func errLocalUserAlreadyExists(username string) error {
	return storeErrorf("local user %s already exists", username)
}

func errNoSuchLocalUser(username string) error {
	return storeErrorf("no such local user %s", username)
}

func errBadLocalRole(s string) error {
	return storeErrorf("bad local role %q", s)
}

func errApiTokenAlreadyExists(prefix string) error {
	return storeErrorf("api token prefix %s already exists", prefix)
}

func errNoSuchApiToken(prefix string) error {
	return storeErrorf("no such api token %s", prefix)
}

func errBadAuditDecision() error {
	return storeErrorf("bad audit decision")
}

func errBadUsageSource(s string) error {
	return storeErrorf("bad usage source %q", s)
}

func errNoSuchAssignment(principal, role, scope string) error {
	return storeErrorf("no such assignment %s/%s/%s", principal, role, scope)
}
