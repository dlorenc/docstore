package types

// Repo-secret events. Plaintext values, ciphertext, nonces, and any sealed
// material MUST NEVER appear on these structs — only metadata that's already
// safe to log: the secret name, its server-assigned id, the size of the
// plaintext (already in size_bytes columns), and the actor or job context.
//
// All four events route through the same broker as everything else and end up
// in event_log + outbox webhooks. SIEM correlation keys off (repo, name); v1
// has a UNIQUE (repo, name) constraint so name is sufficient. The id is
// included for create/update/delete because callers may want to follow a
// specific secret across rotations even after the row has been deleted.

// SecretCreated is emitted when a repo secret is created via
// PUT /-/secrets/{name} (i.e. the row did not previously exist).
type SecretCreated struct {
	Repo      string `json:"repo"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	SizeBytes int    `json:"size_bytes"`
	Actor     string `json:"actor"`
}

func (e SecretCreated) Type() string   { return "com.docstore.secret.created" }
func (e SecretCreated) Source() string { return "/repos/" + e.Repo + "/-/secrets/" + e.Name }
func (e SecretCreated) Data() any      { return e }

// SecretUpdated is emitted when an existing repo secret is overwritten by
// PUT /-/secrets/{name}. The id is preserved across updates, so consumers can
// follow a single secret across rotations.
type SecretUpdated struct {
	Repo      string `json:"repo"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	SizeBytes int    `json:"size_bytes"`
	Actor     string `json:"actor"`
}

func (e SecretUpdated) Type() string   { return "com.docstore.secret.updated" }
func (e SecretUpdated) Source() string { return "/repos/" + e.Repo + "/-/secrets/" + e.Name }
func (e SecretUpdated) Data() any      { return e }

// SecretDeleted is emitted when a repo secret is deleted via
// DELETE /-/secrets/{name}. The id is the id the row carried at delete time
// so SIEM can correlate the delete with prior created/updated events.
type SecretDeleted struct {
	Repo  string `json:"repo"`
	Name  string `json:"name"`
	ID    string `json:"id"`
	Actor string `json:"actor"`
}

func (e SecretDeleted) Type() string   { return "com.docstore.secret.deleted" }
func (e SecretDeleted) Source() string { return "/repos/" + e.Repo + "/-/secrets/" + e.Name }
func (e SecretDeleted) Data() any      { return e }

// SecretAccessed is emitted when the worker resolves a secret at CI dispatch
// time via /-/secrets/reveal. Fired once per successfully revealed name.
//
// v1 does NOT include the secret id — the {repo, name} pair is unique in v1
// (UNIQUE constraint on (repo, name)), so this is sufficient for SIEM
// correlation. v2 may add id once secret versioning lands.
type SecretAccessed struct {
	Repo     string `json:"repo"`
	Name     string `json:"name"`
	JobID    string `json:"job_id"`
	Sequence int64  `json:"sequence"`
	Branch   string `json:"branch"`
}

func (e SecretAccessed) Type() string   { return "com.docstore.secret.accessed" }
func (e SecretAccessed) Source() string { return "/repos/" + e.Repo + "/-/secrets/" + e.Name }
func (e SecretAccessed) Data() any      { return e }
