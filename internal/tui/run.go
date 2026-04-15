package tui

import (
	"bytes"
	"io"
	"net/http"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the Bubble Tea TUI with the given client.
func Run(httpClient *http.Client, remote, repo, author string) error {
	c := &tuiClient{
		httpClient: httpClient,
		remote:     remote,
		repo:       repo,
		author:     author,
	}
	m := newTopModel(c)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// jsonBody converts a byte slice into an io.Reader.
func jsonBody(data []byte) io.Reader {
	return bytes.NewReader(data)
}
