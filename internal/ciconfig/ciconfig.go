// Package ciconfig parses the .docstore/ci.yaml DSL and evaluates trigger
// conditions. It is used by the ci-scheduler to decide whether a push event
// should enqueue a CI job.
package ciconfig

import "github.com/gobwas/glob"

// CIConfig is the top-level structure of .docstore/ci.yaml.
// Only the 'on:' and 'permissions:' blocks are parsed here; execution-related
// fields (checks, jobs) are handled separately by the executor package.
type CIConfig struct {
	On          *TriggerConfig `yaml:"on"`
	Permissions *Permissions   `yaml:"permissions"`
}

// Permissions declares the elevated API permissions a CI job is granted.
// All values are the literal string "write" when present; absent keys are not
// granted. Default (no permissions block): only checks: write is effective.
type Permissions struct {
	Contents  string `yaml:"contents"`  // commit, branch, merge, rebase, purge
	Checks    string `yaml:"checks"`    // check run reporting (default)
	Proposals string `yaml:"proposals"` // create proposals, post reviews/comments
	Issues    string `yaml:"issues"`    // create/close/comment on issues
	Releases  string `yaml:"releases"`  // create/delete releases
	CI        string `yaml:"ci"`        // trigger CI runs
}

// EffectivePermissions returns the list of permission names granted by this
// config. If no Permissions block is declared, only "checks" is returned.
// When a Permissions block is present, only explicitly declared "write"
// permissions are included.
func (cfg *CIConfig) EffectivePermissions() []string {
	if cfg.Permissions == nil {
		return []string{"checks"}
	}
	var perms []string
	if cfg.Permissions.Checks == "write" {
		perms = append(perms, "checks")
	}
	if cfg.Permissions.Contents == "write" {
		perms = append(perms, "contents")
	}
	if cfg.Permissions.Proposals == "write" {
		perms = append(perms, "proposals")
	}
	if cfg.Permissions.Issues == "write" {
		perms = append(perms, "issues")
	}
	if cfg.Permissions.Releases == "write" {
		perms = append(perms, "releases")
	}
	if cfg.Permissions.CI == "write" {
		perms = append(perms, "ci")
	}
	return perms
}

// ScheduleEntry holds a single cron-based schedule trigger.
type ScheduleEntry struct {
	Cron string `yaml:"cron"`
}

// TriggerConfig holds the trigger definitions for a CI config.
type TriggerConfig struct {
	Push     *PushTrigger     `yaml:"push"`
	Proposal *ProposalTrigger `yaml:"proposal"`
	Schedule []ScheduleEntry  `yaml:"schedule"`
}

// PushTrigger configures which branches a push event triggers CI for.
type PushTrigger struct {
	// Branches is a list of glob patterns. A nil or empty list means all branches.
	Branches []string `yaml:"branches"`
}

// ProposalTrigger configures which proposals trigger CI runs.
type ProposalTrigger struct {
	// BaseBranches is a list of glob patterns for target base branches.
	// A nil or empty list means all base branches.
	BaseBranches []string `yaml:"base_branches"`
}

// MatchesPush reports whether a commit to branch should trigger a push-based CI run.
//
// Rules:
//   - No on: block (cfg.On == nil)       → always match (backward compat)
//   - on: exists, no push: key           → never match
//   - on: push: with no branches list    → always match (all branches)
//   - on: push: branches: [pat, ...]     → match if branch matches any pattern
//
// Patterns are evaluated using gobwas/glob which supports **, *, ? and
// character classes.
func (cfg *CIConfig) MatchesPush(branch string) bool {
	if cfg.On == nil {
		return true
	}
	if cfg.On.Push == nil {
		return false
	}
	if len(cfg.On.Push.Branches) == 0 {
		return true
	}
	for _, pattern := range cfg.On.Push.Branches {
		g, err := glob.Compile(pattern)
		if err != nil {
			// Skip invalid patterns rather than treating them as a match.
			continue
		}
		if g.Match(branch) {
			return true
		}
	}
	return false
}

// MatchesProposal reports whether a proposal targeting baseBranch should trigger a CI run.
//
// Rules:
//   - No on: block (cfg.On == nil)             → always match (backward compat)
//   - on: exists, no proposal: key             → never match
//   - on: proposal: with no base_branches list → always match (all base branches)
//   - on: proposal: base_branches: [pat, ...]  → match if baseBranch matches any pattern
//
// Patterns are evaluated using gobwas/glob which supports **, *, ? and
// character classes.
func (cfg *CIConfig) MatchesProposal(baseBranch string) bool {
	if cfg.On == nil {
		return true
	}
	if cfg.On.Proposal == nil {
		return false
	}
	if len(cfg.On.Proposal.BaseBranches) == 0 {
		return true
	}
	for _, pattern := range cfg.On.Proposal.BaseBranches {
		g, err := glob.Compile(pattern)
		if err != nil {
			// Skip invalid patterns rather than treating them as a match.
			continue
		}
		if g.Match(baseBranch) {
			return true
		}
	}
	return false
}
