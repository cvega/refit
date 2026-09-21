package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStagingURLGuards(t *testing.T) {
	for _, api := range []string{"http://api.github.com", "https://evil.test", "https://api.github.com?token=secret", "https://user:secret@api.github.com"} {
		if _, err := stagingURL(Staging{TargetAPI: api, Organization: "org", Repository: "repo-staging"}); err == nil {
			t.Fatalf("accepted %s", api)
		}
	}
	for _, repository := range []string{"production", "../repo-staging", "repo-staging?token=secret"} {
		if _, err := stagingURL(Staging{TargetAPI: "https://api.github.com", Organization: "org", Repository: repository}); err == nil {
			t.Fatal("unsafe target accepted")
		}
	}
	if result, err := stagingURL(Staging{TargetAPI: "https://api.customer.ghe.com", Organization: "org", Repository: "repo-staging"}); err != nil || result != "https://customer.ghe.com/org/repo-staging.git" {
		t.Fatalf("%s %v", result, err)
	}
}

func TestLFSPushExplicitObjectInventory(t *testing.T) {
	gitTestLFS(t)
	fixture := gitTestFixture(t)
	report, err := RewriteGit(context.Background(), fixture.repo, 512, fixture.mapPath)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	var uploads atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == "PUT" && strings.HasPrefix(request.URL.Path, "/payload/") {
			if request.Header.Get("Authorization") != "" {
				t.Error("repository authorization leaked to storage path")
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				writer.WriteHeader(500)
				return
			}
			digest := sha256.Sum256(body)
			if hex.EncodeToString(digest[:]) != strings.TrimPrefix(request.URL.Path, "/payload/") {
				t.Error("uploaded bytes do not match pointer hash")
			}
			uploads.Add(1)
			writer.WriteHeader(200)
			return
		}
		if request.Method != "POST" || request.URL.Path != "/org/repo-staging.git/info/lfs/objects/batch" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(400)
			return
		}
		user, password, ok := request.BasicAuth()
		if !ok || user != "x-access-token" || password != "test-token" {
			t.Error("missing scoped authentication")
			writer.WriteHeader(401)
			return
		}
		var batch struct {
			Operation string `json:"operation"`
			Objects   []struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"objects"`
		}
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			t.Error(err)
			writer.WriteHeader(400)
			return
		}
		if batch.Operation != "upload" || len(batch.Objects) != len(report.LFSObjects) {
			t.Error("incorrect object inventory")
		}
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		var objects []map[string]any
		for _, object := range batch.Objects {
			objects = append(objects, map[string]any{"oid": object.OID, "size": object.Size, "actions": map[string]any{"upload": map[string]string{"href": server.URL + "/payload/" + object.OID}}})
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"objects": objects})
	}))
	defer server.Close()
	if err := pushLFSPayloads(context.Background(), fixture.repo, server.URL+"/org/repo-staging.git", "test-token", report.LFSObjects); err != nil {
		t.Fatal(err)
	}
	if requests.Load() == 0 {
		t.Fatal("LFS upload did not contact batch API")
	}
	if uploads.Load() != int64(len(report.LFSObjects)) {
		t.Fatal("LFS payload uploads incomplete")
	}
}
