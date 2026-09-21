package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	apiUploadChunkSize = 16 << 20
	apiResponseLimit   = 4 << 20
	apiStreamTimeout   = 30 * time.Minute
)

// API implements the REST export, GitHub-owned storage, and GraphQL GEI APIs.
// BaseURL defaults to https://api.github.com; GHES may use /api/v3.
// UploadURL defaults to https://uploads.github.com. Client transports are
// preserved, but cookies and automatic authenticated redirects are not used.
// Plain HTTP is permitted only for loopback hosts with an explicit Client.
type API struct {
	BaseURL    string
	UploadURL  string
	Token      string
	Client     *http.Client
	APIVersion string // Defaults to 2022-11-28 for GHES compatibility.
}

func (a *API) validateURL(u *url.URL) error {
	if u == nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return errors.New("invalid API URL")
	}
	if a.Token != "" && strings.Contains(u.String(), a.Token) {
		return errors.New("credential in API URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme == "http" && a.Client != nil && (strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return errors.New("API URLs must use HTTPS")
}

func (a *API) baseURL(upload bool) (*url.URL, error) {
	raw := a.BaseURL
	if raw == "" {
		raw = "https://api.github.com"
	}
	if upload {
		raw = a.UploadURL
		if raw == "" {
			raw = "https://uploads.github.com"
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid API base URL")
	}
	if err := a.validateURL(u); err != nil {
		return nil, err
	}
	if u.RawQuery != "" || u.ForceQuery || u.RawPath != "" {
		return nil, errors.New("invalid API base URL")
	}
	if upload && strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("upload base URL must be an origin")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func apiSegment(value string) (string, error) {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\r\n") {
		return "", errors.New("invalid organization or repository name")
	}
	return url.PathEscape(value), nil
}

func (a *API) endpoint(path string) (string, error) {
	u, err := a.baseURL(false)
	if err != nil {
		return "", err
	}
	return u.String() + path, nil
}

func (a *API) client() *http.Client {
	c := &http.Client{Timeout: apiStreamTimeout}
	if a.Client != nil {
		c.Transport = a.Client.Transport
		if a.Client.Timeout > 0 {
			c.Timeout = a.Client.Timeout
		}
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func apiIOError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s failed", operation)
}

// request never follows redirects, even if the caller's Client would do so.
// In particular, an archive's storage request cannot inherit authorization,
// cookies, or a Referer containing a signed query from the REST request.
func (a *API) request(ctx context.Context, method, raw string, body io.Reader, authenticated bool, contentType string) (*http.Response, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid request URL")
	}
	if err := a.validateURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("cannot construct API request")
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+a.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		version := a.APIVersion
		if version == "" {
			version = "2022-11-28"
		}
		req.Header.Set("X-GitHub-Api-Version", version)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, apiIOError(ctx, method+" request")
	}
	return resp, nil
}

func apiStatus(resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s request failed (HTTP %d)", resp.Request.Method, resp.StatusCode)
	}
	return nil
}

func apiDecode(ctx context.Context, resp *http.Response, result any) error {
	defer resp.Body.Close()
	if err := apiStatus(resp); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, apiResponseLimit+1))
	if err != nil {
		return apiIOError(ctx, "read API response")
	}
	if len(data) > apiResponseLimit || json.Unmarshal(data, result) != nil {
		return errors.New("invalid API response")
	}
	return nil
}

func (a *API) jsonRequest(ctx context.Context, method, endpoint string, input, result any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return errors.New("cannot encode API request")
		}
		body = bytes.NewReader(data)
	}
	resp, err := a.request(ctx, method, endpoint, body, true, "application/json")
	if err != nil {
		return err
	}
	return apiDecode(ctx, resp, result)
}

func (a *API) migrationURL(org string, id int64, suffix string) (string, error) {
	segment, err := apiSegment(org)
	if err != nil {
		return "", err
	}
	path := "/orgs/" + segment + "/migrations"
	if id != 0 {
		if id < 0 {
			return "", errors.New("invalid migration ID")
		}
		path += "/" + strconv.FormatInt(id, 10)
	}
	return a.endpoint(path + suffix)
}

// Export starts exactly one export; non-idempotent operations are never retried.
func (a *API) Export(ctx context.Context, org, repo, kind string) (int64, error) {
	if _, err := apiSegment(repo); err != nil {
		return 0, err
	}
	input := map[string]any{"repositories": []string{org + "/" + repo}, "lock_repositories": false}
	switch kind {
	case "git":
		input["exclude_metadata"] = true
	case "metadata":
		input["exclude_git_data"] = true
		input["exclude_releases"] = false
		input["exclude_owner_projects"] = true
	default:
		return 0, errors.New("export kind must be git or metadata")
	}
	endpoint, err := a.migrationURL(org, 0, "")
	if err != nil {
		return 0, err
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := a.jsonRequest(ctx, http.MethodPost, endpoint, input, &result); err != nil {
		return 0, err
	}
	if result.ID <= 0 {
		return 0, errors.New("export response missing migration ID")
	}
	return result.ID, nil
}

func (a *API) ExportStatus(ctx context.Context, org string, id int64) (string, error) {
	if id <= 0 {
		return "", errors.New("invalid migration ID")
	}
	endpoint, err := a.migrationURL(org, id, "")
	if err != nil {
		return "", err
	}
	var result struct {
		State string `json:"state"`
	}
	if err := a.jsonRequest(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return "", err
	}
	switch result.State {
	case "pending", "exporting", "exported", "failed":
		return result.State, nil
	default:
		return "", errors.New("invalid export state")
	}
}

func apiRedirect(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

func (a *API) location(current, location string) (*url.URL, error) {
	if location == "" {
		return nil, errors.New("response missing Location")
	}
	base, err := url.Parse(current)
	if err != nil {
		return nil, errors.New("invalid redirect URL")
	}
	relative, err := url.Parse(location)
	if err != nil {
		return nil, errors.New("invalid redirect URL")
	}
	next := base.ResolveReference(relative)
	if err := a.validateURL(next); err != nil {
		return nil, err
	}
	return next, nil
}

// Download streams to an exclusively created file. Existing files are never
// overwritten, and every unsuccessful download removes its partial file.
func (a *API) Download(ctx context.Context, org string, id int64, dest string) (err error) {
	if id <= 0 {
		return errors.New("invalid migration ID")
	}
	endpoint, err := a.migrationURL(org, id, "/archive")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot exclusively create archive file")
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(dest)
		}
	}()
	for redirects := 0; ; redirects++ {
		if redirects > 10 {
			return errors.New("too many archive redirects")
		}
		resp, requestErr := a.request(ctx, http.MethodGet, endpoint, nil, redirects == 0, "")
		if requestErr != nil {
			return requestErr
		}
		if apiRedirect(resp.StatusCode) {
			next, locationErr := a.location(endpoint, resp.Header.Get("Location"))
			_ = resp.Body.Close()
			if locationErr != nil {
				return locationErr
			}
			endpoint = next.String()
			continue
		}
		if statusErr := apiStatus(resp); statusErr != nil {
			_ = resp.Body.Close()
			return statusErr
		}
		// A partial-content or no-content success is not a complete archive.
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return errors.New("unexpected archive response status")
		}
		_, copyErr := io.Copy(f, resp.Body)
		_ = resp.Body.Close()
		if copyErr != nil {
			return apiIOError(ctx, "download archive")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.Close() != nil {
			return errors.New("cannot close archive file")
		}
		return nil
	}
}

func (a *API) uploadLocation(origin *url.URL, prefix, current, location string) (string, error) {
	next, err := a.location(current, location)
	if err != nil {
		return "", err
	}
	if next.Scheme != origin.Scheme || !strings.EqualFold(next.Host, origin.Host) || next.RawPath != "" || strings.Contains(next.Path, "\\") {
		return "", errors.New("untrusted upload Location")
	}
	if next.Path != prefix && !strings.HasPrefix(next.Path, prefix+"/") {
		return "", errors.New("untrusted upload Location")
	}
	for _, segment := range strings.Split(next.Path, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, "%") {
			return "", errors.New("untrusted upload Location")
		}
	}
	return next.String(), nil
}

func (a *API) archiveURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "gei" || u.Host != "archive" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	id := strings.TrimPrefix(u.Path, "/")
	if id == "" || id == "." || id == ".." || (a.Token != "" && strings.Contains(raw, a.Token)) {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

// Upload uses GitHub-owned storage's POST/PATCH/PUT protocol. Each PATCH is at
// most 16 MiB, and every returned Location is validated before sending a PAT.
func (a *API) Upload(ctx context.Context, orgID int64, file string) (string, error) {
	if orgID <= 0 {
		return "", errors.New("invalid organization ID")
	}
	origin, err := a.baseURL(true)
	if err != nil {
		return "", err
	}
	f, err := os.Open(file)
	if err != nil {
		return "", errors.New("cannot open upload file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("upload file must be a regular file")
	}
	prefix := "/organizations/" + strconv.FormatInt(orgID, 10) + "/gei/archive/blobs/uploads"
	endpoint := origin.String() + prefix
	data, _ := json.Marshal(map[string]any{"content_type": "application/octet-stream", "name": info.Name(), "size": info.Size()})
	resp, err := a.request(ctx, http.MethodPost, endpoint, bytes.NewReader(data), true, "application/json")
	if err != nil {
		return "", err
	}
	location := resp.Header.Get("Location")
	var result struct {
		URI string `json:"uri"`
	}
	if err := apiDecode(ctx, resp, &result); err != nil {
		return "", err
	}
	if !a.archiveURI(result.URI) {
		return "", errors.New("invalid archive URI")
	}
	endpoint, err = a.uploadLocation(origin, prefix, endpoint, location)
	if err != nil {
		return "", err
	}
	buffer := make([]byte, apiUploadChunkSize)
	for remaining := info.Size(); remaining > 0; {
		size := int64(len(buffer))
		if remaining < size {
			size = remaining
		}
		if _, err := io.ReadFull(f, buffer[:size]); err != nil {
			return "", apiIOError(ctx, "read upload file")
		}
		resp, err := a.request(ctx, http.MethodPatch, endpoint, bytes.NewReader(buffer[:size]), true, "application/octet-stream")
		if err != nil {
			return "", err
		}
		statusErr := apiStatus(resp)
		location = resp.Header.Get("Location")
		_ = resp.Body.Close()
		if statusErr != nil {
			return "", statusErr
		}
		endpoint, err = a.uploadLocation(origin, prefix, endpoint, location)
		if err != nil {
			return "", err
		}
		remaining -= size
	}
	// Detect a file that grew after the advertised size was obtained.
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
		return "", errors.New("upload file changed while reading")
	}
	resp, err = a.request(ctx, http.MethodPut, endpoint, http.NoBody, true, "application/octet-stream")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := apiStatus(resp); err != nil {
		return "", err
	}
	return result.URI, nil
}

func (a *API) graphql(ctx context.Context, query string, variables any, result any) error {
	u, err := a.baseURL(false)
	if err != nil {
		return err
	}
	if strings.HasSuffix(u.Path, "/api/v3") {
		u.Path = strings.TrimSuffix(u.Path, "/api/v3") + "/api/graphql"
	} else {
		u.Path += "/graphql"
	}
	var envelope struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := a.jsonRequest(ctx, http.MethodPost, u.String(), map[string]any{"query": query, "variables": variables}, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return errors.New("GraphQL request failed")
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, result) != nil {
		return errors.New("invalid GraphQL response")
	}
	return nil
}

func (a *API) Organization(ctx context.Context, org string) (nodeID string, databaseID int64, err error) {
	var result struct {
		Organization struct {
			ID         string `json:"id"`
			DatabaseID int64  `json:"databaseId"`
		} `json:"organization"`
	}
	err = a.graphql(ctx, `query Organization($login: String!) { organization(login: $login) { id databaseId } }`, map[string]any{"login": org}, &result)
	if err != nil {
		return "", 0, err
	}
	if !a.validNodeID(result.Organization.ID) || result.Organization.DatabaseID <= 0 {
		return "", 0, errors.New("organization response missing IDs")
	}
	return result.Organization.ID, result.Organization.DatabaseID, nil
}

func (a *API) validNodeID(id string) bool {
	return id != "" && !strings.ContainsAny(id, ":/\\\r\n\t ") && (a.Token == "" || !strings.Contains(id, a.Token))
}

func (a *API) CreateSource(ctx context.Context, name, sourceURL, ownerID string) (string, error) {
	var result struct {
		CreateMigrationSource struct {
			MigrationSource struct {
				ID string `json:"id"`
			} `json:"migrationSource"`
		} `json:"createMigrationSource"`
	}
	input := map[string]any{"name": name, "url": sourceURL, "ownerId": ownerID, "type": "GITHUB_ARCHIVE"}
	err := a.graphql(ctx, `mutation CreateSource($input: CreateMigrationSourceInput!) { createMigrationSource(input: $input) { migrationSource { id } } }`, map[string]any{"input": input}, &result)
	if err != nil {
		return "", err
	}
	id := result.CreateMigrationSource.MigrationSource.ID
	if !a.validNodeID(id) {
		return "", errors.New("migration source response missing ID")
	}
	return id, nil
}

func (a *API) StartImport(ctx context.Context, sourceID, ownerID, repoName, sourceRepoURL, gitURI, metadataURI, sourceToken string) (string, error) {
	var result struct {
		StartRepositoryMigration struct {
			RepositoryMigration struct {
				ID string `json:"id"`
			} `json:"repositoryMigration"`
		} `json:"startRepositoryMigration"`
	}
	input := map[string]any{
		"sourceId": sourceID, "ownerId": ownerID, "repositoryName": repoName,
		"sourceRepositoryUrl": sourceRepoURL, "gitArchiveUrl": gitURI, "metadataArchiveUrl": metadataURI,
		"targetRepoVisibility": "private", "continueOnError": false,
		"githubPat": a.Token, "accessToken": sourceToken,
	}
	err := a.graphql(ctx, `mutation StartImport($input: StartRepositoryMigrationInput!) { startRepositoryMigration(input: $input) { repositoryMigration { id } } }`, map[string]any{"input": input}, &result)
	if err != nil {
		return "", err
	}
	id := result.StartRepositoryMigration.RepositoryMigration.ID
	if !a.validNodeID(id) || (sourceToken != "" && strings.Contains(id, sourceToken)) {
		return "", errors.New("import response missing migration ID")
	}
	return id, nil
}

func (a *API) ImportStatus(ctx context.Context, id string) (state, failure string, err error) {
	var result struct {
		Node struct {
			State         string `json:"state"`
			FailureReason string `json:"failureReason"`
		} `json:"node"`
	}
	err = a.graphql(ctx, `query ImportStatus($id: ID!) { node(id: $id) { ... on Migration { state failureReason } } }`, map[string]any{"id": id}, &result)
	if err != nil {
		return "", "", err
	}
	switch result.Node.State {
	case "NOT_STARTED", "QUEUED", "IN_PROGRESS", "SUCCEEDED", "FAILED", "FAILED_VALIDATION", "PENDING_VALIDATION", "VALIDATING", "WAITING":
	default:
		return "", "", errors.New("invalid import state")
	}
	if result.Node.FailureReason != "" {
		// Server failure text may echo arbitrary source credentials or signed URLs.
		// This stateless client cannot reliably redact secrets it does not know.
		failure = "migration failure details redacted"
	}
	return result.Node.State, failure, nil
}

func (a *API) RepositoryExists(ctx context.Context, org, repo string) (bool, error) {
	orgPath, err := apiSegment(org)
	if err != nil {
		return false, err
	}
	repoPath, err := apiSegment(repo)
	if err != nil {
		return false, err
	}
	endpoint, err := a.endpoint("/repos/" + orgPath + "/" + repoPath)
	if err != nil {
		return false, err
	}
	resp, err := a.request(ctx, http.MethodGet, endpoint, nil, true, "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err := apiStatus(resp); err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, errors.New("unexpected repository response status")
	}
	return true, nil
}
