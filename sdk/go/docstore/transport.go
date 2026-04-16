package docstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// reqParams accumulates query-string values set by request options.
type reqParams struct {
	query url.Values
}

func (p *reqParams) ensure() {
	if p.query == nil {
		p.query = url.Values{}
	}
}

func (p *reqParams) set(key, value string) {
	p.ensure()
	p.query.Set(key, value)
}

// doJSON issues method path with optional JSON body and decodes a successful
// response into out (pass nil for 204-style responses). All requests go
// through this path so header and query construction live in one place.
//
// path may include a leading slash or not; it is joined to the client base
// URL. query may be nil. method is one of "GET", "POST", "PUT", "PATCH",
// "DELETE".
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.baseURL
	if !strings.HasPrefix(path, "/") {
		u += "/"
	}
	u += path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("docstore: marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return fmt.Errorf("docstore: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	if c.identity != "" {
		req.Header.Set("X-DocStore-Identity", c.identity)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docstore: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || resp.StatusCode == http.StatusNoContent {
			// Drain for connection reuse.
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("docstore: decode response: %w", err)
		}
		return nil
	}
	return decodeError(resp)
}

// repoPath builds "/repos/{owner}/{name}/-/<suffix>" for a repo whose full
// path may itself contain slashes (e.g. "acme/team/subrepo" — owner=acme,
// name=team/subrepo). Each path segment is escaped individually.
func repoPath(repo, suffix string) string {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	base := "/repos/" + strings.Join(parts, "/") + "/-"
	if suffix == "" {
		return base
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return base + suffix
}

// repoBareCRUD builds "/repos/{owner}/{name}" (no "/-/" separator) for the
// handful of endpoints that address the repo itself: GET/DELETE.
func repoBareCRUD(repo string) string {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/repos/" + strings.Join(parts, "/")
}
