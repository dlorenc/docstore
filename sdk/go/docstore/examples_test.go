package docstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/dlorenc/docstore/api"
	"github.com/dlorenc/docstore/sdk/go/docstore"
)

// stubServer is used by the examples below so they can run as standard
// go-test examples (with expected output) without requiring a live
// docstore server.
func stubServer(respStatus int, body any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(respStatus)
		if body != nil && respStatus != http.StatusNoContent {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

// ExampleNewClient shows the minimal client construction pattern from the
// issue that requested this SDK.
func ExampleNewClient() {
	srv := stubServer(http.StatusOK, api.FileResponse{
		Path: "config.yaml", Content: []byte("version: 2\n"),
	})
	defer srv.Close()

	client, err := docstore.NewClient(srv.URL,
		docstore.WithBearerToken("token-from-env"),
	)
	if err != nil {
		panic(err)
	}

	file, err := client.Repo("acme/platform").File(context.Background(),
		"config.yaml", docstore.AtHead("main"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s: %s", file.Path, string(file.Content))
	// Output: config.yaml: version: 2
}

// ExampleRepoClient_Commit shows committing files on a branch.
func ExampleRepoClient_Commit() {
	srv := stubServer(http.StatusCreated, api.CommitResponse{Sequence: 7})
	defer srv.Close()

	client, _ := docstore.NewClient(srv.URL,
		docstore.WithIdentity("alice@example.com"))

	resp, err := client.Repo("acme/platform").Commit(context.Background(),
		api.CommitRequest{
			Branch:  "main",
			Message: "update config",
			Files: []api.FileChange{
				{Path: "config.yaml", Content: []byte("version: 2\n")},
			},
		})
	if err != nil {
		panic(err)
	}
	fmt.Println("committed as sequence", resp.Sequence)
	// Output: committed as sequence 7
}
