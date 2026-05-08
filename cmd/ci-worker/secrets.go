package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/executor"
)

// secretsRevealTimeout caps the round trip to the docstore reveal endpoint.
const secretsRevealTimeout = 30 * time.Second

// revealRequest is the JSON body sent to POST /repos/{repo}/-/secrets/reveal.
type revealRequest struct {
	Names []string `json:"names"`
}

// revealResponse is the JSON body returned by the reveal endpoint on 200.
// Values are base64-encoded plaintext (standard padding).
type revealResponse struct {
	Values  map[string]string `json:"values"`
	Missing []string          `json:"missing"`
}

// fetchSecrets resolves the union of LocalName→plaintext for every secret
// requested by any check in cfg. Returns (resolved, missing, err).
//
// resolved maps LocalName → plaintext bytes. missing is the list of RepoNames
// (not LocalNames) that the server reported as not configured for the repo;
// callers may choose to fail the run or proceed step-by-step.
//
// Returns (nil, nil, nil) if no checks request secrets — saves a round trip.
//
// Errors never include any value bytes — only structural information about
// the response (status code, optional non-sensitive body for 4xx/5xx).
func fetchSecrets(
	ctx context.Context,
	httpc *http.Client,
	docstoreURL, repo, requestToken string,
	cfg *executor.Config,
) (resolved map[string][]byte, missing []string, err error) {
	if cfg == nil {
		return nil, nil, nil
	}

	// Build the deduped, sorted RepoName set across every check.
	repoNameSet := make(map[string]struct{})
	for _, check := range cfg.Checks {
		for _, sec := range check.Secrets {
			if sec.RepoName == "" {
				continue
			}
			repoNameSet[sec.RepoName] = struct{}{}
		}
	}
	if len(repoNameSet) == 0 {
		return nil, nil, nil
	}
	names := make([]string, 0, len(repoNameSet))
	for n := range repoNameSet {
		names = append(names, n)
	}
	sort.Strings(names)

	// Defensive: detect cross-check LocalName conflicts. ValidateSecretsAllowlist
	// already rejects duplicates within a single check; this guards the case
	// where check1 has FOO->A and check2 has FOO->B (currently legal at parse
	// time but ambiguous when collapsing to a single LocalName→plaintext map).
	localOwner := make(map[string]string)
	for _, check := range cfg.Checks {
		for _, sec := range check.Secrets {
			if sec.LocalName == "" {
				continue
			}
			if existing, ok := localOwner[sec.LocalName]; ok && existing != sec.RepoName {
				return nil, nil, fmt.Errorf(
					"local secret name %q maps to both repo names %q and %q across checks",
					sec.LocalName, existing, sec.RepoName,
				)
			}
			localOwner[sec.LocalName] = sec.RepoName
		}
	}

	ctx, cancel := context.WithTimeout(ctx, secretsRevealTimeout)
	defer cancel()

	body, err := json.Marshal(revealRequest{Names: names})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal reveal request: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/-/secrets/reveal", strings.TrimRight(docstoreURL, "/"), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build reveal request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+requestToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("reveal request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, classifyRevealError(resp)
	}

	var rr revealResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, nil, fmt.Errorf("decode reveal response: %w", err)
	}

	// Decode each base64 value into a RepoName→plaintext map. Use a separate
	// scope so the byte slices are never logged or echoed in error strings.
	repoPlaintext := make(map[string][]byte, len(rr.Values))
	for name, encoded := range rr.Values {
		decoded, decErr := base64.StdEncoding.DecodeString(encoded)
		if decErr != nil {
			return nil, nil, fmt.Errorf("decode reveal value for %q: invalid base64", name)
		}
		repoPlaintext[name] = decoded
	}

	// Build the LocalName→plaintext mapping by iterating cfg.Checks. We've
	// already verified that any LocalName collisions resolve to the same
	// RepoName, so duplicate writes are safe.
	resolved = make(map[string][]byte)
	for _, check := range cfg.Checks {
		for _, sec := range check.Secrets {
			pt, ok := repoPlaintext[sec.RepoName]
			if !ok {
				// Missing secrets are reported via the response's `missing`
				// list — not an error. Phase 5 decides whether to fail.
				continue
			}
			resolved[sec.LocalName] = pt
		}
	}

	return resolved, rr.Missing, nil
}

// classifyRevealError converts a non-200 reveal response into a clear error.
// Reads up to 1 KiB of the response body for diagnostic context on 4xx/5xx,
// since the server returns structured error messages there. Never includes
// any potentially sensitive value bytes.
func classifyRevealError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("reveal: unauthorized")
	case http.StatusForbidden:
		body := readBodySnippet(resp.Body)
		if body != "" {
			return fmt.Errorf("reveal: forbidden: %s", body)
		}
		return fmt.Errorf("reveal: forbidden (secrets_blocked)")
	case http.StatusNotFound:
		return fmt.Errorf("reveal: repo or job mismatch")
	}
	body := readBodySnippet(resp.Body)
	if resp.StatusCode >= 500 {
		if body != "" {
			return fmt.Errorf("reveal: server error %d: %s", resp.StatusCode, body)
		}
		return fmt.Errorf("reveal: server error %d", resp.StatusCode)
	}
	if body != "" {
		return fmt.Errorf("reveal: bad request %d: %s", resp.StatusCode, body)
	}
	return fmt.Errorf("reveal: bad request %d", resp.StatusCode)
}

// readBodySnippet returns up to 1 KiB of the response body as a trimmed string.
// Used purely for diagnostic context in error wrapping. Capped to bound the
// blast radius if the server ever echoes back unexpected content.
func readBodySnippet(r io.Reader) string {
	const cap = 1024
	data, _ := io.ReadAll(io.LimitReader(r, cap))
	return strings.TrimSpace(string(data))
}
