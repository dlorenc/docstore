package tui

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// debugEnvVar names the env var that, when set to a writable file path,
// enables TUI debug logging (HTTP requests, response status, decode errors).
// Bubble Tea owns stdout/stderr in alt-screen mode, so logs must go to a file.
const debugEnvVar = "DS_TUI_DEBUG"

// Run starts the Bubble Tea TUI with the given client.
func Run(httpClient *http.Client, remote, repo, author string) error {
	debug, closeDebug, err := newDebugLogger()
	if err != nil {
		return err
	}
	if closeDebug != nil {
		defer closeDebug()
	}

	c := &tuiClient{
		httpClient: httpClient,
		remote:     remote,
		repo:       repo,
		author:     author,
		debug:      debug,
	}
	if debug != nil {
		debug.Printf("tui: starting (remote=%s repo=%s author=%s)", remote, repo, author)
	}
	m := newTopModel(c)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, runErr := p.Run()
	return runErr
}

// newDebugLogger opens the debug log file named by DS_TUI_DEBUG, if set. The
// returned close func is non-nil when a file was opened and must be called to
// release it.
func newDebugLogger() (*log.Logger, func() error, error) {
	path := os.Getenv(debugEnvVar)
	if path == "" {
		return nil, nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s=%q: %w", debugEnvVar, path, err)
	}
	var w io.Writer = f
	return log.New(w, "", log.LstdFlags|log.Lmicroseconds), f.Close, nil
}

// jsonBody converts a byte slice into an io.Reader.
func jsonBody(data []byte) io.Reader {
	return bytes.NewReader(data)
}
