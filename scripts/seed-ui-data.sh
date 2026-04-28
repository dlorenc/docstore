#!/usr/bin/env bash
# scripts/seed-ui-data.sh — Populate a local docstore instance with realistic
# demo data so every UI page has something to show. Safe to re-run (idempotent).
#
# Usage:
#   DATABASE_URL=postgres://docstore:docstore@localhost:5432/docstore?sslmode=disable ./scripts/seed-ui-data.sh
#
# Start the server first (in another terminal):
#   DEV_IDENTITY=dev@example.com \
#   BOOTSTRAP_ADMIN=dev@example.com \
#   DATABASE_URL=postgres://docstore:docstore@localhost:5432/docstore?sslmode=disable \
#   go run ./cmd/docstore
#
# BOOTSTRAP_ADMIN must match IDENTITY (default: dev@example.com) so the script
# can assign its own admin role on the freshly-created demo/app repo.

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
IDENTITY="${IDENTITY:-dev@example.com}"
REPO="demo/app"
ORG="demo"

# ── helpers ──────────────────────────────────────────────────────────────────

# json_field KEY - extract a top-level scalar from stdin JSON
json_field() {
  python3 -c "import json,sys; v=json.load(sys.stdin).get('$1',''); print(v)"
}

# tree_version_id BRANCH FILE - get version_id for FILE from branch tree
tree_version_id() {
  local branch="$1" file="$2"
  curl -sf "$BASE_URL/repos/$REPO/-/tree?branch=${branch}" | \
    python3 -c "
import json, sys
entries = json.load(sys.stdin)
for e in entries:
    if e.get('path') == '$file':
        print(e.get('version_id', ''))
        break
"
}

# http_status METHOD PATH [body] - returns only the HTTP status code
http_status() {
  local method="$1" path="$2" data="${3:-}"
  if [ -n "$data" ]; then
    curl -s -o /dev/null -w "%{http_code}" -X "$method" \
      "$BASE_URL$path" -H "Content-Type: application/json" -d "$data"
  else
    curl -s -o /dev/null -w "%{http_code}" -X "$method" "$BASE_URL$path"
  fi
}

# api_call METHOD PATH [body] - execute a request; die on non-2xx
api_call() {
  local method="$1" path="$2" data="${3:-}"
  local tmp status
  tmp=$(mktemp)
  if [ -n "$data" ]; then
    status=$(curl -s -o "$tmp" -w "%{http_code}" -X "$method" \
      "$BASE_URL$path" -H "Content-Type: application/json" -d "$data")
  else
    status=$(curl -s -o "$tmp" -w "%{http_code}" -X "$method" "$BASE_URL$path")
  fi
  local body
  body=$(cat "$tmp"); rm -f "$tmp"
  if [[ "${status:0:1}" != "2" ]]; then
    echo "  [error] HTTP $status: $body" >&2
    exit 1
  fi
  echo "$body"
}

# try_create DESC METHOD PATH [body]
#   Status messages go to stderr; JSON body goes to stdout (empty on skip).
#   Exits on unexpected errors.
try_create() {
  local desc="$1" method="$2" path="$3" data="${4:-}"
  local tmp status
  tmp=$(mktemp)
  if [ -n "$data" ]; then
    status=$(curl -s -o "$tmp" -w "%{http_code}" -X "$method" \
      "$BASE_URL$path" -H "Content-Type: application/json" -d "$data")
  else
    status=$(curl -s -o "$tmp" -w "%{http_code}" -X "$method" "$BASE_URL$path")
  fi
  local body
  body=$(cat "$tmp"); rm -f "$tmp"
  case "$status" in
    2*)
      echo "  [created] $desc" >&2
      echo "$body"
      ;;
    409)
      echo "  [skip]    $desc already exists" >&2
      ;;
    *)
      echo "  [error]   $desc: HTTP $status: $body" >&2
      exit 1
      ;;
  esac
}

# branch_exists BRANCH - returns 0 if branch exists in REPO
branch_exists() {
  [ "$(http_status GET "/repos/$REPO/-/branch/$1")" = "200" ]
}

# branch_has_commits BRANCH - returns 0 if branch head > base (has commits)
branch_has_commits() {
  local resp head base
  resp=$(curl -sf "$BASE_URL/repos/$REPO/-/branch/$1" 2>/dev/null) || return 1
  head=$(echo "$resp" | python3 -c "import json,sys; print(json.load(sys.stdin).get('head_sequence',0))" 2>/dev/null || echo 0)
  base=$(echo "$resp" | python3 -c "import json,sys; print(json.load(sys.stdin).get('base_sequence',0))" 2>/dev/null || echo 0)
  [ "$head" -gt "$base" ]
}

# commit_to BRANCH MESSAGE FILE_CHANGES_JSON - commit files to a branch
commit_to() {
  local branch="$1" message="$2" files="$3"
  api_call POST "/repos/$REPO/-/commit" \
    "{\"branch\":\"$branch\",\"message\":\"$message\",\"files\":$files}" \
    >/dev/null
  echo "  [committed] $message → $branch"
}

# ── preflight ────────────────────────────────────────────────────────────────

echo "==> Checking server at $BASE_URL..."
if ! curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "  [error] Server not responding at $BASE_URL" >&2
  echo "  Start: DEV_IDENTITY=dev@example.com BOOTSTRAP_ADMIN=dev@example.com DATABASE_URL=... go run ./cmd/docstore" >&2
  exit 1
fi
echo "  [ok]"

if ! command -v python3 >/dev/null 2>&1; then
  echo "  [error] python3 is required for JSON parsing" >&2
  exit 1
fi

# ── org & repo ───────────────────────────────────────────────────────────────

echo ""
echo "==> Org: $ORG"
try_create "org $ORG" POST /orgs "{\"name\":\"$ORG\"}"

echo ""
echo "==> Repo: $REPO"
try_create "repo $REPO" POST /repos "{\"owner\":\"demo\",\"name\":\"app\"}"

echo ""
echo "==> Role: $IDENTITY → admin on $REPO"
# PUT /roles/:identity is idempotent (200 OK, not 409); always apply.
status=$(http_status PUT "/repos/$REPO/-/roles/$IDENTITY" "{\"role\":\"admin\"}")
case "$status" in
  200) echo "  [set]     admin role for $IDENTITY" ;;
  403) echo "  [skip]    role already set (no bootstrap admin access)" ;;
  *)   echo "  [error]   role set HTTP $status" >&2; exit 1 ;;
esac

# ── branches ─────────────────────────────────────────────────────────────────

echo ""
echo "==> Branches"

for BNAME in "feature/auth" "feature/logging" "feature/old-thing" "experiment/broken"; do
  if branch_exists "$BNAME"; then
    echo "  [skip]    branch $BNAME already exists"
  else
    api_call POST "/repos/$REPO/-/branch" "{\"name\":\"$BNAME\"}" >/dev/null
    echo "  [created] branch $BNAME"
  fi
done

# ── commits: feature/auth (3 commits) ────────────────────────────────────────

echo ""
echo "==> Commits: feature/auth"

if branch_has_commits "feature/auth"; then
  echo "  [skip]    feature/auth already has commits"
else
  commit_to "feature/auth" "feat: add JWT authentication middleware" \
    '[{"path":"src/auth.go","content":"cGFja2FnZSBhdXRoCgppbXBvcnQgImVycm9ycyIKCi8vIFZhbGlkYXRlSldUIHZhbGlkYXRlcyBhbiBKV1QgdG9rZW4uCmZ1bmMgVmFsaWRhdGVKV1QodG9rZW4gc3RyaW5nKSBlcnJvciB7CglpZiB0b2tlbiA9PSAiIiB7CgkJcmV0dXJuIGVycm9ycy5OZXcoImVtcHR5IHRva2VuIikKCX0KCXJldHVybiBuaWwKfQo="}]'

  commit_to "feature/auth" "feat: add user model and store" \
    '[{"path":"src/user.go","content":"cGFja2FnZSBhdXRoCgovLyBVc2VyIHJlcHJlc2VudHMgYW4gYXV0aGVudGljYXRlZCB1c2VyLgp0eXBlIFVzZXIgc3RydWN0IHsKCUlEICAgIHN0cmluZwogICAgRW1haWwgc3RyaW5nCn0K"}]'

  commit_to "feature/auth" "docs: update README with auth setup instructions" \
    '[{"path":"README.md","content":"IyBBdXRoIFNlcnZpY2UKClRoaXMgcGFja2FnZSBwcm92aWRlcyBKV1QtYmFzZWQgYXV0aGVudGljYXRpb24uCgojIyBVc2FnZQoKYGBgZ28KZXJyIDo9IGF1dGguVmFsaWRhdGVKV1QodG9rZW4pCmBgYAo="}]'
fi

# ── commits: feature/logging (2 commits) ─────────────────────────────────────

echo ""
echo "==> Commits: feature/logging"

if branch_has_commits "feature/logging"; then
  echo "  [skip]    feature/logging already has commits"
else
  commit_to "feature/logging" "feat: add structured logger" \
    '[{"path":"src/logger.go","content":"cGFja2FnZSBsb2dnZXIKCmltcG9ydCAibG9nL3Nsb2ciCgovLyBOZXcgcmV0dXJucyBhIHN0cnVjdHVyZWQgc2xvZyBsb2dnZXIuCmZ1bmMgTmV3KCkgKnNsb2cuTG9nZ2VyIHsKCXJldHVybiBzbG9nLkRlZmF1bHQoKQp9Cg=="}]'

  commit_to "feature/logging" "feat: add logger config from environment" \
    '[{"path":"src/config.go","content":"cGFja2FnZSBsb2dnZXIKCmltcG9ydCAib3MiCgovLyBMZXZlbEZyb21FbnYgcmVhZHMgTE9HX0xFVkVMIGZyb20gdGhlIGVudmlyb25tZW50LgpmdW5jIExldmVsRnJvbUVudigpIHN0cmluZyB7CglyZXR1cm4gb3MuR2V0ZW52KCJMT0dfTEVWRUwiKQp9Cg=="}]'
fi

# ── commits + merge: feature/old-thing ───────────────────────────────────────

echo ""
echo "==> Commits + merge: feature/old-thing"

if branch_has_commits "feature/old-thing"; then
  echo "  [skip]    feature/old-thing already has commits (may already be merged)"
else
  commit_to "feature/old-thing" "feat: add legacy thing module" \
    '[{"path":"src/thing.go","content":"cGFja2FnZSB0aGluZwoKLy8gVGhpbmcgaXMgYSBsZWdhY3kgY29tcG9uZW50LgpmdW5jIFRoaW5nKCkgc3RyaW5nIHsgcmV0dXJuICJvbGQtdGhpbmciIH0K"}]'
fi

if [ "$(http_status GET "/repos/$REPO/-/branch/feature/old-thing")" = "200" ]; then
  branch_status=$(curl -sf "$BASE_URL/repos/$REPO/-/branch/feature/old-thing" | \
    python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  if [ "$branch_status" = "merged" ]; then
    echo "  [skip]    feature/old-thing already merged"
  else
    merge_resp=$(curl -s -o /tmp/_merge_resp -w "%{http_code}" \
      -X POST "$BASE_URL/repos/$REPO/-/merge" \
      -H "Content-Type: application/json" \
      -d '{"branch":"feature/old-thing"}')
    merge_body=$(cat /tmp/_merge_resp); rm -f /tmp/_merge_resp
    if [[ "$merge_resp" == 2* ]]; then
      echo "  [merged]  feature/old-thing into main"
    else
      echo "  [warn]    merge feature/old-thing: HTTP $merge_resp: $merge_body" >&2
    fi
  fi
fi

# ── commits + abandon: experiment/broken ─────────────────────────────────────

echo ""
echo "==> Commits + abandon: experiment/broken"

if branch_has_commits "experiment/broken"; then
  echo "  [skip]    experiment/broken already has commits (may already be abandoned)"
else
  commit_to "experiment/broken" "wip: broken experiment — do not merge" \
    '[{"path":"src/broken.go","content":"cGFja2FnZSBtYWluCgppbXBvcnQgImZtdCIKCi8vIFRPRE86IHRoaXMgZG9lc24ndCB3b3JrCmZ1bmMgbWFpbigpIHsgZm10LlByaW50bG4oImJyb2tlbiIpIH0K"}]'
fi

if [ "$(http_status GET "/repos/$REPO/-/branch/experiment/broken")" = "200" ]; then
  branch_status=$(curl -sf "$BASE_URL/repos/$REPO/-/branch/experiment/broken" | \
    python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  if [ "$branch_status" = "abandoned" ]; then
    echo "  [skip]    experiment/broken already abandoned"
  else
    del_status=$(http_status DELETE "/repos/$REPO/-/branch/experiment/broken")
    if [[ "$del_status" == 2* ]]; then
      echo "  [abandoned] experiment/broken"
    else
      echo "  [warn]    abandon experiment/broken: HTTP $del_status" >&2
    fi
  fi
fi

# ── issues ───────────────────────────────────────────────────────────────────

echo ""
echo "==> Issues"

# Check how many issues already exist
existing_count=$(curl -sf "$BASE_URL/repos/$REPO/-/issues?state=all" 2>/dev/null | \
  python3 -c "import json,sys; print(len(json.load(sys.stdin).get('issues',[])))" 2>/dev/null || echo 0)

if [ "$existing_count" -ge 4 ]; then
  echo "  [skip]    issues already seeded ($existing_count found)"
else
  # Capture JSON bodies (try_create sends status lines to stderr, body to stdout)
  body=$(try_create "issue: Bug: login fails on Safari" \
    POST "/repos/$REPO/-/issues" \
    '{"title":"Bug: login fails on Safari","body":"Users on Safari 17 get a 401 when using the login form. Likely a cookie SameSite issue.","labels":["bug","P1"]}')
  [ -n "$body" ] && echo "$body" | python3 -c "import json,sys; d=json.load(sys.stdin); print('  issue #'+str(d.get('number','?')))" 2>/dev/null || true

  body=$(try_create "issue: Feature: add dark mode" \
    POST "/repos/$REPO/-/issues" \
    '{"title":"Feature: add dark mode","body":"Add a dark mode toggle to the UI. Should persist across sessions via localStorage.","labels":["enhancement","UX"]}')
  [ -n "$body" ] && echo "$body" | python3 -c "import json,sys; d=json.load(sys.stdin); print('  issue #'+str(d.get('number','?')))" 2>/dev/null || true

  body=$(try_create "issue: Docs: update README" \
    POST "/repos/$REPO/-/issues" \
    '{"title":"Docs: update README","body":"The README is out of date. Update the quickstart and add a contributing guide.","labels":["documentation"]}')
  [ -n "$body" ] && echo "$body" | python3 -c "import json,sys; d=json.load(sys.stdin); print('  issue #'+str(d.get('number','?')))" 2>/dev/null || true

  issue4_body=$(try_create "issue: Chore: upgrade dependencies" \
    POST "/repos/$REPO/-/issues" \
    '{"title":"Chore: upgrade dependencies","body":"Run go get -u ./... and vendor the changes.","labels":["chore"]}')
  issue4_num=$([ -n "$issue4_body" ] && echo "$issue4_body" | python3 -c "import json,sys; print(json.load(sys.stdin).get('number',''))" 2>/dev/null || echo "")

  if [ -n "$issue4_num" ]; then
    close_status=$(http_status POST "/repos/$REPO/-/issues/$issue4_num/close" \
      '{"reason":"completed"}')
    if [[ "$close_status" == 2* ]]; then
      echo "  [closed]  issue #$issue4_num (chore: upgrade dependencies)"
    else
      echo "  [warn]    close issue #$issue4_num: HTTP $close_status" >&2
    fi
  fi
fi

# ── proposal ─────────────────────────────────────────────────────────────────

echo ""
echo "==> Proposal: feature/auth → main"

existing_proposals=$(curl -sf "$BASE_URL/repos/$REPO/-/proposals?state=open" 2>/dev/null | \
  python3 -c "
import json, sys
data = json.load(sys.stdin)
# list proposals returns a bare array
ps = data if isinstance(data, list) else data.get('proposals', [])
for p in ps:
    if p.get('branch') == 'feature/auth':
        print(p.get('id',''))
        break
" 2>/dev/null || echo "")

if [ -n "$existing_proposals" ]; then
  echo "  [skip]    proposal for feature/auth already exists (id=$existing_proposals)"
  PROPOSAL_ID="$existing_proposals"
else
  proposal_body=$(api_call POST "/repos/$REPO/-/proposals" \
    '{"branch":"feature/auth","base_branch":"main","title":"feat: JWT authentication","description":"Adds JWT-based auth middleware, user model, and updated README.\n\nCloses #1"}')
  PROPOSAL_ID=$(echo "$proposal_body" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  echo "  [created] proposal $PROPOSAL_ID"
fi

# ── review ───────────────────────────────────────────────────────────────────

echo ""
echo "==> Review: approve feature/auth"

# Always create review — reviews don't have 409 semantics; one review per run is fine
review_body=$(api_call POST "/repos/$REPO/-/review" \
  '{"branch":"feature/auth","status":"approved","body":"LGTM! Auth implementation looks solid. JWT validation is correct and the user model is clean."}')
REVIEW_ID=$(echo "$review_body" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
echo "  [created] review $REVIEW_ID (approved)"

# ── review comment ───────────────────────────────────────────────────────────

echo ""
echo "==> Review comment on src/auth.go"

VERSION_ID=$(tree_version_id "feature/auth" "src/auth.go")
if [ -z "$VERSION_ID" ]; then
  echo "  [warn]    could not get version_id for src/auth.go — skipping comment"
else
  comment_data="{\"branch\":\"feature/auth\",\"path\":\"src/auth.go\",\"version_id\":\"$VERSION_ID\",\"body\":\"Consider adding token expiry validation here — right now we only check for empty tokens.\"}"
  if [ -n "$REVIEW_ID" ]; then
    comment_data="{\"branch\":\"feature/auth\",\"path\":\"src/auth.go\",\"version_id\":\"$VERSION_ID\",\"review_id\":\"$REVIEW_ID\",\"body\":\"Consider adding token expiry validation here — right now we only check for empty tokens.\"}"
  fi
  comment_body=$(api_call POST "/repos/$REPO/-/comment" "$comment_data")
  echo "  [created] review comment $(echo "$comment_body" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo '')"
fi

# ── releases ─────────────────────────────────────────────────────────────────

echo ""
echo "==> Releases"

# Get current main head sequence (needs at least one commit on main from the merge)
main_head=$(curl -sf "$BASE_URL/repos/$REPO/-/branch/main" 2>/dev/null | \
  python3 -c "import json,sys; print(json.load(sys.stdin).get('head_sequence',0))" 2>/dev/null || echo 0)

if [ "$main_head" -eq 0 ]; then
  echo "  [warn]    main has no commits — releases need at least one commit on main"
  echo "            (merge feature/old-thing into main first, or ensure a prior merge succeeded)"
else
  # Both releases point to current main head (sufficient for UI demo purposes)
  try_create "release v1.0.0" POST "/repos/$REPO/-/releases" \
    "{\"name\":\"v1.0.0\",\"body\":\"Initial stable release. Includes the legacy thing module.\"}"

  try_create "release v1.1.0" POST "/repos/$REPO/-/releases" \
    "{\"name\":\"v1.1.0\",\"body\":\"Maintenance release. Dependency updates and minor fixes.\"}"
fi

# ── summary ──────────────────────────────────────────────────────────────────

echo ""
echo "==> Seed complete!"
echo ""
echo "    Repo:     http://localhost:8080/ui/repos/$REPO"
echo "    Branches: http://localhost:8080/ui/repos/$REPO/branches"
echo "    Issues:   http://localhost:8080/ui/repos/$REPO/issues"
echo ""
