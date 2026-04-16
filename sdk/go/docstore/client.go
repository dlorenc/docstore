package docstore

import (
	"errors"
	"net/http"
	"strings"
)

// DefaultUserAgent is sent on every request unless overridden via
// WithUserAgent.
const DefaultUserAgent = "docstore-go-sdk/0.1"

// Client is the entry point for all API calls. It is safe for concurrent use.
type Client struct {
	baseURL   string
	http      *http.Client
	userAgent string

	bearerToken string
	identity    string
}

// ClientOption configures a Client constructed with NewClient.
type ClientOption func(*Client)

// NewClient returns a Client targeting the docstore server at baseURL. The
// URL may include a path prefix (e.g. "https://api.example.com/docstore");
// trailing slashes are stripped.
func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("docstore: base URL is required")
	}
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      http.DefaultClient,
		userAgent: DefaultUserAgent,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// WithBearerToken sets the Authorization header to "Bearer <token>" on every
// request. Forward-compatible with API tokens tracked in server issue #101.
func WithBearerToken(token string) ClientOption {
	return func(c *Client) { c.bearerToken = token }
}

// WithIdentity sets the X-DocStore-Identity header on every request. The
// server only honours this header in dev mode (no IAP); use it for local
// development and tests.
func WithIdentity(identity string) ClientOption {
	return func(c *Client) { c.identity = identity }
}

// WithHTTPClient replaces the *http.Client used for all requests. Use it to
// inject timeouts, proxies, or an IAP-authenticated transport.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithUserAgent overrides the User-Agent header. The default is
// DefaultUserAgent.
func WithUserAgent(ua string) ClientOption {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// Repo returns a handle scoped to the repo with full path "owner/name" (e.g.
// "acme/platform"). The handle is cheap to construct; reuse it or create new
// ones as convenient.
func (c *Client) Repo(name string) *RepoClient {
	return &RepoClient{client: c, repo: strings.Trim(name, "/")}
}

// Orgs returns the service for organisation-level endpoints.
func (c *Client) Orgs() *OrgsService { return &OrgsService{client: c} }

// Repos returns the service for repo-CRUD endpoints (distinct from the
// repo-scoped operations exposed by Client.Repo).
func (c *Client) Repos() *ReposService { return &ReposService{client: c} }
