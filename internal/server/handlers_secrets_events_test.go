package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlorenc/docstore/internal/citoken"
	"github.com/dlorenc/docstore/internal/events"
	evtypes "github.com/dlorenc/docstore/internal/events/types"
	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/secrets"
)

// recordingEmitter is an in-memory eventEmitter for handler tests. It records
// every Emit call in order so tests can assert exact event types and payloads.
//
// We use it instead of a real *events.Broker so the secrets handler tests
// don't need Postgres / event_log / outbox plumbing — phase 6 only cares that
// the right events are produced with the right fields, not that they reach
// the broker's persistent layer.
type recordingEmitter struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingEmitter) Emit(_ context.Context, e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingEmitter) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, len(r.events))
	copy(out, r.events)
	return out
}

// buildSecretsServerWithEmitter wires the secrets handler with a fake secrets
// service and a recordingEmitter so tests can assert what was emitted.
func buildSecretsServerWithEmitter(t *testing.T, fake *fakeSecretsService) (http.Handler, *recordingEmitter) {
	t.Helper()
	rec := &recordingEmitter{}
	ms := &mockStore{}
	s := &server{commitStore: ms, secrets: fake, emitter: rec}
	return s.buildHandler(devID, devID, ms), rec
}

// buildRevealServerWithEmitter wires the reveal handler with a fake secrets
// service, a stub job-token store, an optional mockStore (for proposal lookup),
// and a recordingEmitter.
func buildRevealServerWithEmitter(t *testing.T, fake *fakeSecretsService, jobStore jobTokenStore, ms *mockStore) (http.Handler, *recordingEmitter) {
	t.Helper()
	if ms == nil {
		ms = &mockStore{}
	}
	rec := &recordingEmitter{}
	s := &server{
		commitStore:   ms,
		secrets:       fake,
		jobTokenStore: jobStore,
		emitter:       rec,
	}
	return s.buildHandler(devID, devID, ms), rec
}

// assertNoSecretValueInEvents marshals every recorded event's Data field and
// fails the test if the secret value appears anywhere in the JSON. This is
// the regression guard against accidentally adding a Value field to one of
// the secret.* event types.
func assertNoSecretValueInEvents(t *testing.T, rec *recordingEmitter, values ...string) {
	t.Helper()
	for i, e := range rec.snapshot() {
		blob, err := json.Marshal(e.Data())
		if err != nil {
			t.Fatalf("event %d (%s): marshal Data: %v", i, e.Type(), err)
		}
		for _, v := range values {
			if v == "" {
				continue
			}
			if strings.Contains(string(blob), v) {
				t.Fatalf("event %d (%s) contains secret value %q in JSON: %s",
					i, e.Type(), v, blob)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// SetSecret -> SecretCreated / SecretUpdated
// ---------------------------------------------------------------------------

func TestSecretsSet_EmitsSecretCreated(t *testing.T) {
	const repo = "org/myrepo"
	const name = "DOCKERHUB_TOKEN"
	const value = "the-plaintext-value-do-not-leak"
	now := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	fake := &fakeSecretsService{
		setFn: func(_ context.Context, gotRepo, gotName, _ string, v []byte, actor string) (secrets.Metadata, error) {
			// Brand-new row: UpdatedAt is nil — that's how the handler tells
			// create from update.
			return secrets.Metadata{
				ID:        "sec-abc123",
				Repo:      gotRepo,
				Name:      gotName,
				SizeBytes: len(v),
				CreatedBy: actor,
				CreatedAt: now,
			}, nil
		},
	}
	h, rec := buildSecretsServerWithEmitter(t, fake)
	_ = t.Context()

	body := fmt.Sprintf(`{"value":%q}`, value)
	res := doSecretsRequest(t, h, http.MethodPut, "/repos/"+repo+"/-/secrets/"+name, body)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT secret: status=%d body=%s", res.Code, res.Body.String())
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("emitted %d events; want 1: %#v", len(got), got)
	}
	created, ok := got[0].(evtypes.SecretCreated)
	if !ok {
		t.Fatalf("event type: got %T (%s), want SecretCreated", got[0], got[0].Type())
	}
	if created.Repo != repo || created.Name != name {
		t.Errorf("SecretCreated: repo=%q name=%q, want %q/%q", created.Repo, created.Name, repo, name)
	}
	if created.ID != "sec-abc123" {
		t.Errorf("SecretCreated.ID = %q, want sec-abc123", created.ID)
	}
	if created.SizeBytes != len(value) {
		t.Errorf("SecretCreated.SizeBytes = %d, want %d", created.SizeBytes, len(value))
	}
	if created.Actor != devID {
		t.Errorf("SecretCreated.Actor = %q, want %q", created.Actor, devID)
	}
	if created.Type() != "com.docstore.secret.created" {
		t.Errorf("Type() = %q", created.Type())
	}
	wantSrc := "/repos/" + repo + "/-/secrets/" + name
	if created.Source() != wantSrc {
		t.Errorf("Source() = %q, want %q", created.Source(), wantSrc)
	}

	assertNoSecretValueInEvents(t, rec, value)
}

func TestSecretsSet_EmitsSecretUpdated(t *testing.T) {
	const repo = "org/myrepo"
	const name = "DOCKERHUB_TOKEN"
	const value = "rotated-value-must-not-leak"
	now := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	updatedBy := "alice@example.com"
	updatedAt := now.Add(time.Hour)
	fake := &fakeSecretsService{
		setFn: func(_ context.Context, gotRepo, gotName, _ string, v []byte, _ string) (secrets.Metadata, error) {
			// Existing row: UpdatedAt non-nil. The handler must read this
			// pointer (not the actor) to decide create vs. update.
			return secrets.Metadata{
				ID:        "sec-xyz789",
				Repo:      gotRepo,
				Name:      gotName,
				SizeBytes: len(v),
				CreatedBy: "creator@example.com",
				CreatedAt: now,
				UpdatedBy: &updatedBy,
				UpdatedAt: &updatedAt,
			}, nil
		},
	}
	h, rec := buildSecretsServerWithEmitter(t, fake)

	body := fmt.Sprintf(`{"value":%q}`, value)
	res := doSecretsRequest(t, h, http.MethodPut, "/repos/"+repo+"/-/secrets/"+name, body)
	if res.Code != http.StatusOK {
		t.Fatalf("PUT secret: status=%d body=%s", res.Code, res.Body.String())
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("emitted %d events; want 1: %#v", len(got), got)
	}
	updated, ok := got[0].(evtypes.SecretUpdated)
	if !ok {
		t.Fatalf("event type: got %T (%s), want SecretUpdated", got[0], got[0].Type())
	}
	if updated.Repo != repo || updated.Name != name || updated.ID != "sec-xyz789" {
		t.Errorf("SecretUpdated fields: %+v", updated)
	}
	if updated.SizeBytes != len(value) {
		t.Errorf("SecretUpdated.SizeBytes = %d, want %d", updated.SizeBytes, len(value))
	}
	if updated.Actor != devID {
		t.Errorf("SecretUpdated.Actor = %q, want %q", updated.Actor, devID)
	}
	if updated.Type() != "com.docstore.secret.updated" {
		t.Errorf("Type() = %q", updated.Type())
	}

	assertNoSecretValueInEvents(t, rec, value)
}

func TestSecretsSet_ValidationFailures_EmitNothing(t *testing.T) {
	const value = "should-never-leak"
	cases := []struct {
		name string
		err  error
	}{
		{"invalid_name", secrets.ErrInvalidName},
		{"reserved", secrets.ErrReservedName},
		{"too_large", secrets.ErrValueTooLarge},
		{"empty", secrets.ErrEmptyValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSecretsService{
				setFn: func(_ context.Context, _, _, _ string, _ []byte, _ string) (secrets.Metadata, error) {
					return secrets.Metadata{}, tc.err
				},
			}
			h, rec := buildSecretsServerWithEmitter(t, fake)
			_ = t.Context()

			body := fmt.Sprintf(`{"value":%q}`, value)
			res := doSecretsRequest(t, h, http.MethodPut, "/repos/org/myrepo/-/secrets/X", body)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("got status=%d body=%s", res.Code, res.Body.String())
			}
			if got := rec.snapshot(); len(got) != 0 {
				t.Fatalf("emitted %d events on validation failure; want 0: %#v", len(got), got)
			}
		})
	}
}

func TestSecretsSet_ServiceError_EmitsNothing(t *testing.T) {
	fake := &fakeSecretsService{
		setFn: func(_ context.Context, _, _, _ string, _ []byte, _ string) (secrets.Metadata, error) {
			return secrets.Metadata{}, errors.New("kms is on fire")
		},
	}
	h, rec := buildSecretsServerWithEmitter(t, fake)
	_ = t.Context()

	res := doSecretsRequest(t, h, http.MethodPut, "/repos/org/myrepo/-/secrets/X", `{"value":"v"}`)
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("got status=%d body=%s", res.Code, res.Body.String())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("emitted %d events on service error; want 0: %#v", len(got), got)
	}
}

// ---------------------------------------------------------------------------
// DeleteSecret -> SecretDeleted
// ---------------------------------------------------------------------------

func TestSecretsDelete_EmitsSecretDeleted(t *testing.T) {
	const repo = "org/myrepo"
	const name = "MY_SECRET"
	fake := &fakeSecretsService{
		deleteFn: func(_ context.Context, gotRepo, gotName string) (secrets.Metadata, error) {
			return secrets.Metadata{
				ID:   "sec-delete-1",
				Repo: gotRepo,
				Name: gotName,
			}, nil
		},
	}
	h, rec := buildSecretsServerWithEmitter(t, fake)
	_ = t.Context()

	res := doSecretsRequest(t, h, http.MethodDelete, "/repos/"+repo+"/-/secrets/"+name, "")
	if res.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d body=%s", res.Code, res.Body.String())
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("emitted %d events; want 1: %#v", len(got), got)
	}
	deleted, ok := got[0].(evtypes.SecretDeleted)
	if !ok {
		t.Fatalf("event type: got %T (%s), want SecretDeleted", got[0], got[0].Type())
	}
	if deleted.Repo != repo || deleted.Name != name || deleted.ID != "sec-delete-1" {
		t.Errorf("SecretDeleted fields: %+v", deleted)
	}
	if deleted.Actor != devID {
		t.Errorf("SecretDeleted.Actor = %q, want %q", deleted.Actor, devID)
	}
	if deleted.Type() != "com.docstore.secret.deleted" {
		t.Errorf("Type() = %q", deleted.Type())
	}
}

func TestSecretsDelete_NotFound_EmitsNothing(t *testing.T) {
	fake := &fakeSecretsService{
		deleteFn: func(_ context.Context, _, _ string) (secrets.Metadata, error) {
			return secrets.Metadata{}, secrets.ErrNotFound
		},
	}
	h, rec := buildSecretsServerWithEmitter(t, fake)
	_ = t.Context()

	res := doSecretsRequest(t, h, http.MethodDelete, "/repos/org/myrepo/-/secrets/MISSING", "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("got status=%d body=%s", res.Code, res.Body.String())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("emitted %d events; want 0: %#v", len(got), got)
	}
}

// ---------------------------------------------------------------------------
// RevealSecrets -> SecretAccessed
// ---------------------------------------------------------------------------

func TestRevealSecrets_EmitsOnePerRevealedName(t *testing.T) {
	const repo = "org/myrepo"
	const dockerVal = "docker-creds-do-not-leak"
	const slackVal = "slack-creds-also-do-not-leak"
	plaintext, _, err := citoken.GenerateRequestToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, names []string) (map[string][]byte, []string, error) {
			vals := map[string][]byte{}
			for _, n := range names {
				switch n {
				case "DOCKERHUB_TOKEN":
					vals[n] = []byte(dockerVal)
				case "SLACK_INCOMING":
					vals[n] = []byte(slackVal)
				}
			}
			return vals, nil, nil
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID:          "job-evt-1",
		Repo:        repo,
		Branch:      "main",
		Sequence:    42,
		TriggerType: "push",
	})

	h, rec := buildRevealServerWithEmitter(t, fake, jobStore, nil)
	res := postReveal(t, h, t.Context(), repo, plaintext,
		`{"names":["DOCKERHUB_TOKEN","SLACK_INCOMING"]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("POST reveal: status=%d body=%s", res.Code, res.Body.String())
	}

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("emitted %d events; want 2: %#v", len(got), got)
	}
	// Iteration order matches request order: DOCKERHUB_TOKEN first, SLACK_INCOMING second.
	expected := []string{"DOCKERHUB_TOKEN", "SLACK_INCOMING"}
	for i, want := range expected {
		ev, ok := got[i].(evtypes.SecretAccessed)
		if !ok {
			t.Fatalf("event %d: got %T (%s), want SecretAccessed", i, got[i], got[i].Type())
		}
		if ev.Repo != repo {
			t.Errorf("event %d: Repo = %q, want %q", i, ev.Repo, repo)
		}
		if ev.Name != want {
			t.Errorf("event %d: Name = %q, want %q", i, ev.Name, want)
		}
		if ev.JobID != "job-evt-1" {
			t.Errorf("event %d: JobID = %q, want job-evt-1", i, ev.JobID)
		}
		if ev.Sequence != 42 {
			t.Errorf("event %d: Sequence = %d, want 42", i, ev.Sequence)
		}
		if ev.Branch != "main" {
			t.Errorf("event %d: Branch = %q, want main", i, ev.Branch)
		}
		if ev.Type() != "com.docstore.secret.accessed" {
			t.Errorf("event %d: Type() = %q", i, ev.Type())
		}
	}

	assertNoSecretValueInEvents(t, rec, dockerVal, slackVal)
}

func TestRevealSecrets_OneFoundOneMissing_EmitsOnlyForFound(t *testing.T) {
	const repo = "org/myrepo"
	plaintext, _, _ := citoken.GenerateRequestToken()

	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, names []string) (map[string][]byte, []string, error) {
			// Only "FOUND" is revealed; "MISSING" is reported missing.
			vals := map[string][]byte{"FOUND": []byte("v")}
			missing := []string{"MISSING"}
			_ = names
			return vals, missing, nil
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "job-evt-2", Repo: repo, Branch: "feature", Sequence: 7,
		TriggerType: "manual",
	})

	h, rec := buildRevealServerWithEmitter(t, fake, jobStore, nil)
	res := postReveal(t, h, t.Context(), repo, plaintext,
		`{"names":["FOUND","MISSING"]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("POST reveal: status=%d body=%s", res.Code, res.Body.String())
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("emitted %d events; want 1 (only the found one): %#v", len(got), got)
	}
	ev, ok := got[0].(evtypes.SecretAccessed)
	if !ok {
		t.Fatalf("event type: got %T", got[0])
	}
	if ev.Name != "FOUND" {
		t.Errorf("event Name = %q, want FOUND", ev.Name)
	}
}

func TestRevealSecrets_DeniedByGating_EmitsNothing(t *testing.T) {
	const repo = "org/myrepo"
	plaintext, _, _ := citoken.GenerateRequestToken()

	propID := "prop-9"
	ms := &mockStore{
		getProposalFn: func(_ context.Context, _, _ string) (*model.Proposal, error) {
			return &model.Proposal{ID: propID, Author: "outsider@example.com"}, nil
		},
		listOrgMembersFn: func(_ context.Context, _ string) ([]model.OrgMember, error) {
			return []model.OrgMember{{Org: "org", Identity: "alice@example.com"}}, nil
		},
	}
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			t.Fatal("Reveal must not be called when gating denies the request")
			return nil, nil, nil
		},
	}
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID:                "job-evt-3",
		Repo:              repo,
		TriggerType:       "proposal",
		TriggerProposalID: &propID,
	})

	h, rec := buildRevealServerWithEmitter(t, fake, jobStore, ms)
	res := postReveal(t, h, t.Context(), repo, plaintext, `{"names":["X"]}`)
	if res.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got status=%d body=%s", res.Code, res.Body.String())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("emitted %d events on gating denial; want 0: %#v", len(got), got)
	}
}

func TestRevealSecrets_InvalidName_EmitsNothing(t *testing.T) {
	plaintext, _, _ := citoken.GenerateRequestToken()
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "job-evt-4", Repo: "org/myrepo", TriggerType: "push",
	})
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			t.Fatal("Reveal must not be called for invalid name")
			return nil, nil, nil
		},
	}
	h, rec := buildRevealServerWithEmitter(t, fake, jobStore, nil)
	res := postReveal(t, h, t.Context(), "org/myrepo", plaintext, `{"names":["lowercase"]}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got status=%d body=%s", res.Code, res.Body.String())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("emitted %d events on invalid name; want 0: %#v", len(got), got)
	}
}

func TestRevealSecrets_ServiceError_EmitsNothing(t *testing.T) {
	plaintext, _, _ := citoken.GenerateRequestToken()
	jobStore := revealJobLookup(t, plaintext, &model.CIJob{
		ID: "job-evt-5", Repo: "org/myrepo", TriggerType: "push",
	})
	fake := &fakeSecretsService{
		revealFn: func(_ context.Context, _ string, _ []string) (map[string][]byte, []string, error) {
			return nil, nil, errors.New("kms unavailable")
		},
	}
	h, rec := buildRevealServerWithEmitter(t, fake, jobStore, nil)
	res := postReveal(t, h, t.Context(), "org/myrepo", plaintext, `{"names":["X"]}`)
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got status=%d body=%s", res.Code, res.Body.String())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("emitted %d events on service error; want 0: %#v", len(got), got)
	}
}
