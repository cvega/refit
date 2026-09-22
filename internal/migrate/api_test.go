package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const apiTestToken = "destination-PAT-secret"

func apiTestServer(t *testing.T, handler http.HandlerFunc) (*API, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return &API{BaseURL: s.URL, UploadURL: s.URL, Token: apiTestToken, Client: s.Client()}, s
}

func apiTestAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+apiTestToken {
		t.Error("missing or incorrect authorization")
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		t.Errorf("API version = %q", got)
	}
	if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q", got)
	}
}

func apiTestJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode request: %v", err)
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Error("expected JSON content type")
	}
	return body
}

func apiTestRedacted(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range append(secrets, apiTestToken) {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Error("error contains sensitive data")
		}
	}
}

func TestAPICheckMigrationAccess(t *testing.T) {
	for _, scenario := range []struct {
		name, scopes, role string
		destination, valid bool
	}{
		{"source", "repo, admin:org", "admin", false, true},
		{"destination", "repo, admin:org, workflow", "admin", true, true},
		{"missing-workflow", "repo, admin:org", "admin", true, false},
		{"missing-scopes", "", "admin", false, false},
		{"not-owner", "repo, admin:org", "member", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			api, _ := apiTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
				apiTestAuth(t, request)
				if request.Method != http.MethodGet {
					t.Fatal("preflight must not write")
				}
				switch request.URL.Path {
				case "/user":
					writer.Header().Set("X-OAuth-Scopes", scenario.scopes)
					fmt.Fprint(writer, `{"login":"operator"}`)
				case "/orgs/example/memberships/operator":
					json.NewEncoder(writer).Encode(map[string]string{"state": "active", "role": scenario.role})
				default:
					t.Errorf("unexpected path %s", request.URL.Path)
				}
			})
			if err := api.CheckMigrationAccess(context.Background(), "example", scenario.destination); (err == nil) != scenario.valid {
				t.Fatalf("unexpected preflight result: %v", err)
			}
		})
	}
}

func TestAPIExport(t *testing.T) {
	for _, kind := range []string{"git", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				apiTestAuth(t, r)
				if r.Method != "POST" || r.URL.Path != "/api/v3/orgs/acme/migrations" {
					t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
				}
				want := map[string]any{"repositories": []any{"acme/widget"}, "lock_repositories": false}
				if kind == "git" {
					want["exclude_metadata"] = true
				} else {
					want["exclude_git_data"] = true
					want["exclude_releases"] = false
					want["exclude_owner_projects"] = true
				}
				if got := apiTestJSON(t, r); !reflect.DeepEqual(got, want) {
					t.Errorf("export payload = %#v, want %#v", got, want)
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"id":1234567890123}`)
			})
			a.BaseURL += "/api/v3/"
			id, err := a.Export(context.Background(), "acme", "widget", kind)
			if err != nil || id != 1234567890123 || calls != 1 {
				t.Fatalf("Export = %d, %v; calls=%d", id, err, calls)
			}
		})
	}
}

func TestAPIExportStatus(t *testing.T) {
	for _, state := range []string{"pending", "exporting", "exported", "failed"} {
		t.Run(state, func(t *testing.T) {
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				apiTestAuth(t, r)
				if r.Method != "GET" || r.URL.Path != "/orgs/acme/migrations/42" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				json.NewEncoder(w).Encode(map[string]string{"state": state})
			})
			got, err := a.ExportStatus(context.Background(), "acme", 42)
			if err != nil || got != state {
				t.Fatalf("ExportStatus = %q, %v", got, err)
			}
		})
	}
}

func TestAPIRepositoryExists(t *testing.T) {
	for _, code := range []int{200, 404, 401, 403, 429, 500, 204, 302} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			calls := 0
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				apiTestAuth(t, r)
				if r.Method != "GET" || r.URL.Path != "/repos/acme/widget" {
					t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(code)
				fmt.Fprint(w, apiTestToken)
			})
			got, err := a.RepositoryExists(context.Background(), "acme", "widget")
			if got != (code == 200) {
				t.Errorf("exists=%v for HTTP %d", got, code)
			}
			if code == 200 || code == 404 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				apiTestRedacted(t, err)
			}
			if calls != 1 {
				t.Errorf("request was retried or redirected: %d calls", calls)
			}
		})
	}
}

func TestAPIDownloadRedirectIsolation(t *testing.T) {
	const payload = "archive content\x00\xff"
	var storageCalls atomic.Int32
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageCalls.Add(1)
		for _, header := range []string{"Authorization", "Cookie", "Referer", "X-GitHub-Api-Version"} {
			if r.Header.Get(header) != "" {
				t.Errorf("storage received %s", header)
			}
		}
		switch r.URL.Path {
		case "/signed":
			if r.URL.Query().Get("signature") != "private-signature" {
				t.Error("signed query not preserved")
			}
			w.Header().Set("Location", "/final?signature=other-signature")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/final":
			fmt.Fprint(w, payload)
		default:
			t.Errorf("unexpected storage path: %s", r.URL.Path)
		}
	}))
	defer storage.Close()
	a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		apiTestAuth(t, r)
		if r.Method != "GET" || r.URL.Path != "/orgs/acme/migrations/42/archive" {
			t.Errorf("unexpected archive path: %s", r.URL.Path)
		}
		w.Header().Set("Location", storage.URL+"/signed?signature=private-signature")
		w.WriteHeader(http.StatusFound)
	})
	a.Client = storage.Client() // Custom TLS transport must survive client isolation.
	a.Client.Jar, _ = cookiejar.New(nil)
	storageURL, _ := url.Parse(storage.URL)
	a.Client.Jar.SetCookies(storageURL, []*http.Cookie{{Name: "credential", Value: apiTestToken}})
	a.Client.CheckRedirect = func(*http.Request, []*http.Request) error {
		t.Error("caller redirect hook must not run")
		return nil
	}
	dest := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := a.Download(context.Background(), "acme", 42, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != payload || storageCalls.Load() != 2 {
		t.Fatalf("download mismatch: err=%v, calls=%d", err, storageCalls.Load())
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("archive mode: info=%v, err=%v", info, err)
	}
}

func TestAPIDownloadSameOriginRedirectDropsAuth(t *testing.T) {
	calls := 0
	a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			apiTestAuth(t, r)
			http.Redirect(w, r, "/signed", http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("same-origin storage redirect inherited PAT")
		}
		fmt.Fprint(w, "archive")
	})
	if err := a.Download(context.Background(), "acme", 1, filepath.Join(t.TempDir(), "archive")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls=%d", calls)
	}
}

func TestAPIDownloadExclusiveCreate(t *testing.T) {
	var calls atomic.Int32
	a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	dir := t.TempDir()
	dest := filepath.Join(dir, "archive")
	if err := os.WriteFile(dest, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dest, dir, filepath.Join(dir, "missing", "archive")} {
		if err := a.Download(context.Background(), "acme", 1, path); err == nil {
			t.Error("expected exclusive-create failure")
		}
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(dest, link); err != nil {
		t.Fatal(err)
	}
	if err := a.Download(context.Background(), "acme", 1, link); err == nil {
		t.Error("followed preexisting symlink")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "keep" || calls.Load() != 0 {
		t.Error("existing file changed or unnecessary request made")
	}
}

func TestAPIDownloadFailureCleanup(t *testing.T) {
	for _, mode := range []string{"status", "partial", "missing-location", "invalid-location", "insecure-location", "redirect-loop", "no-content", "partial-content", "direct"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch mode {
				case "status":
					w.WriteHeader(403)
					fmt.Fprint(w, apiTestToken)
				case "partial":
					w.Header().Set("Content-Length", "1000")
					fmt.Fprint(w, "short")
				case "missing-location":
					w.WriteHeader(302)
				case "invalid-location":
					w.Header().Set("Location", "https://%zz/"+apiTestToken)
					w.WriteHeader(302)
				case "insecure-location":
					w.Header().Set("Location", "http://storage.example/archive?signature=private-signature")
					w.WriteHeader(302)
				case "redirect-loop":
					w.Header().Set("Location", "/loop")
					w.WriteHeader(302)
				case "no-content":
					w.WriteHeader(204)
				case "partial-content":
					w.WriteHeader(206)
				case "direct":
					fmt.Fprint(w, "archive")
				}
			})
			dest := filepath.Join(t.TempDir(), "archive")
			err := a.Download(context.Background(), "acme", 1, dest)
			if mode == "direct" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			apiTestRedacted(t, err, "private-signature", "storage.example")
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("partial archive remains: %v", err)
			}
			if mode == "redirect-loop" && calls != 11 {
				t.Errorf("redirect bound: calls=%d", calls)
			}
		})
	}
}

func TestAPIUploadProtocol(t *testing.T) {
	for _, size := range []int{0, 31, apiUploadChunkSize, apiUploadChunkSize + 127} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x97}, size)
			file := filepath.Join(t.TempDir(), "archive.tar.gz")
			if err := os.WriteFile(file, payload, 0600); err != nil {
				t.Fatal(err)
			}
			prefix := "/organizations/1234567890123/gei/archive/blobs/uploads"
			step, offset := 0, 0
			nextPath := prefix
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				apiTestAuth(t, r)
				if r.URL.RequestURI() != nextPath {
					t.Errorf("upload path=%s, want %s", r.URL.RequestURI(), nextPath)
				}
				if step == 0 {
					if r.Method != "POST" {
						t.Errorf("init method=%s", r.Method)
					}
					want := map[string]any{"content_type": "application/octet-stream", "name": "archive.tar.gz", "size": float64(size)}
					if got := apiTestJSON(t, r); !reflect.DeepEqual(got, want) {
						t.Errorf("init payload=%#v", got)
					}
					nextPath = prefix + "/guid?part=1"
					w.Header().Set("Location", nextPath)
					w.WriteHeader(201)
				} else if offset < size {
					if r.Method != "PATCH" || r.Header.Get("Content-Type") != "application/octet-stream" {
						t.Errorf("chunk method/content type = %s/%s", r.Method, r.Header.Get("Content-Type"))
					}
					chunk, err := io.ReadAll(r.Body)
					wantSize := min(apiUploadChunkSize, size-offset)
					if err != nil || len(chunk) != wantSize || r.ContentLength != int64(wantSize) {
						t.Errorf("chunk len=%d, ContentLength=%d, err=%v", len(chunk), r.ContentLength, err)
					}
					if !bytes.Equal(chunk, payload[offset:offset+wantSize]) {
						t.Error("chunk content mismatch")
					}
					offset += wantSize
					nextPath = prefix + "/guid?part=" + strconv.Itoa(step+1)
					// Exercise both relative and absolute Location forms.
					w.Header().Set("Location", "http://"+r.Host+nextPath)
					w.WriteHeader(202)
				} else {
					if r.Method != "PUT" || r.ContentLength != 0 {
						t.Errorf("final method/length=%s/%d", r.Method, r.ContentLength)
					}
					body, _ := io.ReadAll(r.Body)
					if len(body) != 0 {
						t.Error("final PUT body must be empty")
					}
					w.WriteHeader(201)
					fmt.Fprint(w, `{"uri":"gei://archive/guid"}`)
				}
				step++
			})
			uri, err := a.Upload(context.Background(), 1234567890123, file)
			wantSteps := 2 + (size+apiUploadChunkSize-1)/apiUploadChunkSize
			if err != nil || uri != "gei://archive/guid" || offset != size || step != wantSteps {
				t.Fatalf("Upload=%q, %v; offset=%d, steps=%d (want %d)", uri, err, offset, step, wantSteps)
			}
		})
	}
}

func TestAPIUploadRejectsUntrustedLocations(t *testing.T) {
	var leaked atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer foreign.Close()
	prefix := "/organizations/7/gei/archive/blobs/uploads"
	locations := []string{
		"", foreign.URL + prefix + "/id", "//" + strings.TrimPrefix(foreign.URL, "http://") + prefix,
		"https://uploads.github.com.evil.example" + prefix, "http://storage.example" + prefix,
		"/organizations/8/gei/archive/blobs/uploads/id", prefix + "-evil/id", "/other", "../../other",
		prefix + "/%2e%2e/escape", prefix + "/%252e%252e/escape", prefix + "/%2fother",
		prefix + "/..\\escape", prefix + "/id#fragment", "https://user:password@example.com" + prefix,
		"https://%zz/" + apiTestToken,
	}
	file := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for i, location := range locations {
		for _, stage := range []string{"POST", "PATCH"} {
			t.Run(fmt.Sprintf("%d/%s", i, stage), func(t *testing.T) {
				calls := 0
				a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Method == stage {
						w.Header().Set("Location", location)
					} else {
						w.Header().Set("Location", prefix+"/id")
					}
					fmt.Fprint(w, `{"uri":"gei://archive/guid"}`)
				})
				_, err := a.Upload(context.Background(), 7, file)
				apiTestRedacted(t, err, "password")
				wantCalls := 1
				if stage == "PATCH" {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Errorf("untrusted location followed: calls=%d", calls)
				}
			})
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("upload request reached foreign origin")
	}
}

func TestAPIUploadFailures(t *testing.T) {
	for _, mode := range []string{"POST", "PATCH", "PUT", "bad-json", "missing-uri", "https-uri", "credential-uri", "query-uri", "fragment-uri", "changed-file", "truncated-file"} {
		t.Run(mode, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "archive")
			if err := os.WriteFile(file, []byte("content"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method == mode {
					w.WriteHeader(503)
					fmt.Fprint(w, apiTestToken)
					return
				}
				w.Header().Set("Location", "/organizations/7/gei/archive/blobs/uploads/guid")
				if r.Method == "POST" && (mode == "changed-file" || mode == "truncated-file") {
					newData := []byte("longer content")
					if mode == "truncated-file" {
						newData = nil
					}
					if err := os.WriteFile(file, newData, 0600); err != nil {
						t.Error(err)
					}
				}
				if r.Method != "PUT" {
					return
				}
				switch mode {
				case "bad-json":
					fmt.Fprint(w, apiTestToken)
				case "missing-uri":
					fmt.Fprint(w, `{}`)
				case "https-uri":
					fmt.Fprint(w, `{"uri":"https://storage.example/secret"}`)
				case "credential-uri":
					json.NewEncoder(w).Encode(map[string]string{"uri": "gei://archive/" + apiTestToken})
				case "query-uri":
					fmt.Fprint(w, `{"uri":"gei://archive/guid?secret=value"}`)
				case "fragment-uri":
					fmt.Fprint(w, `{"uri":"gei://archive/guid#secret"}`)
				default:
					fmt.Fprint(w, `{"uri":"gei://archive/guid"}`)
				}
			})
			_, err := a.Upload(context.Background(), 7, file)
			apiTestRedacted(t, err, file, "storage.example")
			if calls > 3 {
				t.Errorf("unexpected retry: calls=%d", calls)
			}
		})
	}
}

func TestAPIGraphQLOperations(t *testing.T) {
	for _, basePath := range []string{"", "/api/v3/"} {
		t.Run(basePath, func(t *testing.T) {
			calls := 0
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				apiTestAuth(t, r)
				path := "/graphql"
				if basePath != "" {
					path = "/api/graphql"
				}
				if r.Method != "POST" || r.URL.Path != path {
					t.Errorf("GraphQL endpoint=%s %s", r.Method, r.URL.Path)
				}
				body := apiTestJSON(t, r)
				query, _ := body["query"].(string)
				variables, _ := body["variables"].(map[string]any)
				switch calls {
				case 1:
					if !strings.Contains(query, "organization(login: $login)") || !strings.Contains(query, "databaseId") || !reflect.DeepEqual(variables, map[string]any{"login": "destination"}) {
						t.Error("incorrect organization query")
					}
					fmt.Fprint(w, `{"data":{"organization":{"id":"O_owner","databaseId":1234567890123}}}`)
				case 2:
					want := map[string]any{"name": "source name", "url": "https://ghes.example", "ownerId": "O_owner", "type": "GITHUB_ARCHIVE"}
					if !strings.Contains(query, "createMigrationSource(input: $input)") || !reflect.DeepEqual(variables["input"], want) {
						t.Error("incorrect source mutation input")
					}
					fmt.Fprint(w, `{"data":{"createMigrationSource":{"migrationSource":{"id":"MS_source"}}}}`)
				case 3:
					want := map[string]any{
						"sourceId": "MS_source", "ownerId": "O_owner", "repositoryName": "staging-repo",
						"sourceRepositoryUrl": "https://ghes.example/source/repo", "gitArchiveUrl": "gei://archive/git",
						"metadataArchiveUrl": "gei://archive/metadata", "targetRepoVisibility": "private",
						"continueOnError": false, "githubPat": apiTestToken, "accessToken": "source-PAT-secret",
					}
					if !strings.Contains(query, "startRepositoryMigration(input: $input)") || !reflect.DeepEqual(variables["input"], want) {
						t.Error("incorrect import mutation input")
					}
					fmt.Fprint(w, `{"data":{"startRepositoryMigration":{"repositoryMigration":{"id":"RM_import"}}}}`)
				case 4:
					if !strings.Contains(query, "... on Migration { state failureReason }") || !reflect.DeepEqual(variables, map[string]any{"id": "RM_import"}) {
						t.Error("incorrect import status query")
					}
					fmt.Fprint(w, `{"data":{"node":{"state":"SUCCEEDED","failureReason":null}}}`)
				default:
					t.Error("unexpected extra GraphQL request")
				}
			})
			a.BaseURL += basePath
			ctx := context.Background()
			node, db, err := a.Organization(ctx, "destination")
			if err != nil || node != "O_owner" || db != 1234567890123 {
				t.Fatalf("Organization=%q,%d,%v", node, db, err)
			}
			source, err := a.CreateSource(ctx, "source name", "https://ghes.example", node)
			if err != nil || source != "MS_source" {
				t.Fatalf("CreateSource=%q,%v", source, err)
			}
			id, err := a.StartImport(ctx, source, node, "staging-repo", "https://ghes.example/source/repo", "gei://archive/git", "gei://archive/metadata", "source-PAT-secret")
			if err != nil || id != "RM_import" {
				t.Fatalf("StartImport=%q,%v", id, err)
			}
			state, failure, err := a.ImportStatus(ctx, id)
			if err != nil || state != "SUCCEEDED" || failure != "" || calls != 4 {
				t.Fatalf("ImportStatus=%q,%q,%v; calls=%d", state, failure, err, calls)
			}
		})
	}
}

func TestAPIGraphQLErrors(t *testing.T) {
	responses := []string{
		`{"errors":[{"message":"` + apiTestToken + ` source-PAT-secret https://signed.example/?secret=x"}]}`,
		`{"data":{"organization":{"id":"O_owner","databaseId":7}},"errors":[{"message":"partial failure"}]}`,
		`{"data":null}`, `{}`, `not JSON ` + apiTestToken,
		`{"data":{"organization":null}}`, `{"data":{"organization":{"id":"O_owner","databaseId":0}}}`,
		`{"data":{"organization":{"id":"` + apiTestToken + `","databaseId":7}}}`,
	}
	for i, response := range responses {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) })
			_, _, err := a.Organization(context.Background(), "destination")
			apiTestRedacted(t, err, "source-PAT-secret", "signed.example", "partial failure")
		})
	}
	for _, method := range []string{"source", "import", "status"} {
		t.Run(method+"-null", func(t *testing.T) {
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{}}`) })
			var err error
			switch method {
			case "source":
				_, err = a.CreateSource(context.Background(), "name", "https://source.example", "owner")
			case "import":
				_, err = a.StartImport(context.Background(), "source", "owner", "repo", "https://source.example/repo", "gei://archive/git", "gei://archive/metadata", "source-PAT-secret")
			case "status":
				_, _, err = a.ImportStatus(context.Background(), "id")
			}
			apiTestRedacted(t, err)
		})
	}
}

func TestAPIImportFailureRedaction(t *testing.T) {
	a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
			"state": "FAILED", "failureReason": apiTestToken + " source-PAT-secret https://signed.example/?signature=secret",
		}}})
	})
	state, failure, err := a.ImportStatus(context.Background(), "RM_import")
	if err != nil || state != "FAILED" || failure != "migration failure details redacted" {
		t.Error("failure reason was not safely redacted")
	}
}

func TestAPIStatusValidation(t *testing.T) {
	for i, state := range []string{"NOT_STARTED", "QUEUED", "IN_PROGRESS", "SUCCEEDED", "FAILED", "FAILED_VALIDATION", "PENDING_VALIDATION", "VALIDATING", "WAITING", "", apiTestToken} {
		t.Run("import-"+strconv.Itoa(i), func(t *testing.T) {
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]string{"state": state}}})
			})
			got, _, err := a.ImportStatus(context.Background(), "id")
			if state == "" || state == apiTestToken {
				apiTestRedacted(t, err)
				if got != "" {
					t.Error("unrecognized state exposed")
				}
			} else if err != nil || got != state {
				t.Errorf("ImportStatus=%q,%v", got, err)
			}
		})
	}
	for i, state := range []string{"", apiTestToken} {
		t.Run("export-"+strconv.Itoa(i), func(t *testing.T) {
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"state": state})
			})
			got, err := a.ExportStatus(context.Background(), "org", 1)
			apiTestRedacted(t, err)
			if got != "" {
				t.Error("unrecognized state exposed")
			}
		})
	}
}

func TestAPIDownloadStreamingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		fmt.Fprint(w, "partial archive")
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	})
	dest := filepath.Join(t.TempDir(), "archive")
	if err := a.Download(ctx, "org", 1, dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("streaming cancellation=%v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("canceled partial file remains")
	}
}

func TestAPIUploadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == "POST" {
			w.Header().Set("Location", "/organizations/7/gei/archive/blobs/uploads/guid")
			fmt.Fprint(w, `{"uri":"gei://archive/guid"}`)
			return
		}
		cancel()
		io.Copy(io.Discard, r.Body)
	})
	file := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := a.Upload(ctx, 7, file)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("upload cancellation=%v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("upload progressed after cancellation: calls=%d", calls.Load())
	}
}

type apiTestRoundTripper func(*http.Request) (*http.Response, error)

func (f apiTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPIHTTPSAndConfiguration(t *testing.T) {
	for _, raw := range []string{"http://api.github.com", "http://127.0.0.2", "ftp://example.com", "https://user:password@example.com", "https://example.com?token=secret", "https://example.com#fragment", "https://%zz", "/relative"} {
		t.Run(raw, func(t *testing.T) {
			a := &API{BaseURL: raw, Token: apiTestToken}
			_, err := a.Export(context.Background(), "org", "repo", "git")
			apiTestRedacted(t, err, "password", "token=secret")
		})
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		a := &API{Client: &http.Client{}}
		u, _ := url.Parse("http://" + host + ":1234")
		if err := a.validateURL(u); err != nil {
			t.Errorf("loopback rejected: %v", err)
		}
		a.Client = nil
		if err := a.validateURL(u); err == nil {
			t.Error("HTTP allowed without explicit Client")
		}
	}
	a := &API{}
	base, err := a.baseURL(false)
	if err != nil || base.String() != "https://api.github.com" {
		t.Fatalf("REST default=%v,%v", base, err)
	}
	base, err = a.baseURL(true)
	if err != nil || base.String() != "https://uploads.github.com" {
		t.Fatalf("upload default=%v,%v", base, err)
	}
	for _, raw := range []string{"http://not-loopback.example", "http://localhost.evil.example", "https://example.com/api"} {
		a.UploadURL, a.Client = raw, &http.Client{}
		if _, err := a.baseURL(true); err == nil {
			t.Error("invalid upload base accepted")
		}
	}
	a, _ = apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Api-Version") != "2099-01-01" {
			t.Error("configured API version ignored")
		}
		fmt.Fprint(w, `{"id":1}`)
	})
	a.APIVersion = "2099-01-01"
	if _, err := a.Export(context.Background(), "org", "repo", "git"); err != nil {
		t.Fatal(err)
	}
	if a.client().Timeout != apiStreamTimeout {
		t.Error("missing streaming timeout")
	}
	a.Client.Timeout = 17 * time.Second
	if a.client().Timeout != 17*time.Second {
		t.Error("custom timeout ignored")
	}
}

func TestAPITransportErrorsAndCancellation(t *testing.T) {
	a := &API{Token: apiTestToken, Client: &http.Client{Transport: apiTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("%s https://signed.example/?signature=secret", apiTestToken)
	})}}
	_, err := a.Export(context.Background(), "org", "repo", "git")
	apiTestRedacted(t, err, "signed.example", "signature=secret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = a.Export(ctx, "org", "repo", "git")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	dest := filepath.Join(t.TempDir(), "archive")
	if err := a.Download(ctx, "org", 1, dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("download cancellation=%v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("canceled archive not removed")
	}
}

func TestAPIValidationAndResponseLimits(t *testing.T) {
	var calls atomic.Int32
	a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	ctx := context.Background()
	if _, err := a.Export(ctx, "org", "repo", "unknown"); err == nil {
		t.Error("invalid export kind accepted")
	}
	for _, segment := range []string{"", ".", "..", "org/repo", "org\\repo", "org\nrepo"} {
		if _, err := a.Export(ctx, segment, "repo", "git"); err == nil {
			t.Error("invalid organization accepted")
		}
		if _, err := a.Export(ctx, "org", segment, "git"); err == nil {
			t.Error("invalid repository accepted")
		}
		if _, err := a.RepositoryExists(ctx, "org", segment); err == nil {
			t.Error("invalid existence repository accepted")
		}
	}
	for _, id := range []int64{0, -1} {
		if _, err := a.ExportStatus(ctx, "org", id); err == nil {
			t.Error("invalid export ID accepted")
		}
		if err := a.Download(ctx, "org", id, filepath.Join(t.TempDir(), "archive")); err == nil {
			t.Error("invalid download ID accepted")
		}
		if _, err := a.Upload(ctx, id, "missing"); err == nil {
			t.Error("invalid upload organization accepted")
		}
	}
	dir := t.TempDir()
	for _, file := range []string{dir, filepath.Join(dir, "missing")} {
		_, err := a.Upload(ctx, 1, file)
		apiTestRedacted(t, err, file)
	}
	if calls.Load() != 0 {
		t.Error("invalid input made network requests")
	}
	for i, response := range []string{`{}`, `{"id":-1}`, `{"id":"` + apiTestToken + `"}`, `{"id":1} trailing`, strings.Repeat("x", apiResponseLimit+1)} {
		t.Run("response-"+strconv.Itoa(i), func(t *testing.T) {
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) })
			_, err := a.Export(ctx, "org", "repo", "git")
			apiTestRedacted(t, err)
		})
	}
}

func TestAPINonIdempotentRequestsNotRetriedOrRedirected(t *testing.T) {
	for _, code := range []int{302, 307, 429, 500, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			calls := 0
			a, _ := apiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/retry")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(code)
				fmt.Fprint(w, apiTestToken)
			})
			_, err := a.Export(context.Background(), "org", "repo", "git")
			apiTestRedacted(t, err)
			_, err = a.CreateSource(context.Background(), "name", "https://source.example", "owner")
			apiTestRedacted(t, err)
			if calls != 2 {
				t.Errorf("non-idempotent operations repeated: calls=%d", calls)
			}
		})
	}
}
