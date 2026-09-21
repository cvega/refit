package migrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MetadataRule selects an RFC6901 pointer. "*" matches ARRAY elements only;
// object keys (including a literal "*") are exact. Empty Path selects the root.
// Actions are commit (direct string/null), preserve (entire subtree), and
// commit-url (full SHA URL path segments only). No field names are inferred.
type MetadataRule struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// MetadataPolicy is opt-in: its zero value and initial JSON configuration are
// unreviewed. Each metadata JSON file needs an exact canonical relative filename
// in Files, even when its reviewed rule list is empty. Git internals are excluded.
type MetadataPolicy struct {
	Reviewed bool                      `json:"reviewed"`
	Files    map[string][]MetadataRule `json:"files"`
}

type metadataSelector struct {
	tokens []string
	action string
}

func pointerTokens(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("JSON pointer must be empty or start with /")
	}
	tokens := strings.Split(pointer[1:], "/")
	for i, token := range tokens {
		var decoded strings.Builder
		for j := 0; j < len(token); j++ {
			if token[j] != '~' {
				decoded.WriteByte(token[j])
				continue
			}
			j++
			if j == len(token) || (token[j] != '0' && token[j] != '1') {
				return nil, errors.New("invalid RFC6901 escape")
			}
			if token[j] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
		}
		tokens[i] = decoded.String()
	}
	return tokens, nil
}

func compileMetadataPolicy(policy MetadataPolicy) (map[string][]metadataSelector, error) {
	if !policy.Reviewed {
		return nil, errors.New("metadata policy must be explicitly reviewed")
	}
	result := make(map[string][]metadataSelector)
	names := make(map[string]string)
	for file, rules := range policy.Files {
		if safe, err := safeRelativePath(file); err != nil || safe != file || file == "." || strings.ContainsAny(file, "*?[]") {
			return nil, fmt.Errorf("metadata policy requires an exact canonical filename: %q", file)
		}
		if !strings.EqualFold(path.Ext(file), ".json") {
			return nil, fmt.Errorf("metadata policy supports only JSON files: %s", file)
		}
		if err := registerArchivePath(names, file); err != nil {
			return nil, err
		}
		result[file] = nil
		seen := make(map[string]bool)
		for _, rule := range rules {
			tokens, err := pointerTokens(rule.Path)
			if err != nil {
				return nil, fmt.Errorf("policy for %s: %w", file, err)
			}
			if rule.Action != "commit" && rule.Action != "preserve" && rule.Action != "commit-url" {
				return nil, fmt.Errorf("unsupported metadata action for %s", file)
			}
			if seen[rule.Path] {
				return nil, fmt.Errorf("duplicate metadata rule for %s at %s", file, rule.Path)
			}
			seen[rule.Path] = true
			result[file] = append(result[file], metadataSelector{tokens: tokens, action: rule.Action})
		}
	}
	return result, nil
}

func metadataPointer(tokens []string) string {
	var result strings.Builder
	for _, token := range tokens {
		result.WriteByte('/')
		result.WriteString(strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1"))
	}
	return result.String()
}

func fullGitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// decodeMetadataValue uses tokens to reject duplicate keys (Decode into a map
// would silently discard them). UseNumber preserves large integers/exponents.
func decodeMetadataValue(dec *json.Decoder, depth int) (any, error) {
	if depth > 512 {
		return nil, errors.New("unsupported JSON nesting deeper than 512")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid JSON object key")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("unsupported duplicate JSON key: %q", key)
			}
			value, err := decodeMetadataValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if end, err := dec.Token(); err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid JSON object terminator")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for dec.More() {
			value, err := decodeMetadataValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := dec.Token(); err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid JSON array terminator")
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

type metadataEdit struct {
	name   string
	before []byte
	after  []byte
	mode   os.FileMode
	staged string
	backup string
}

func stageMetadata(name string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(name), ".metadata-*")
	if err != nil {
		return "", err
	}
	_, writeErr := f.Write(data)
	chmodErr := f.Chmod(mode.Perm())
	if err := errors.Join(writeErr, chmodErr, f.Close()); err != nil {
		return "", errors.Join(err, os.Remove(f.Name()))
	}
	return f.Name(), nil
}

// commitMetadata stages ALL replacements and rollback copies before touching an
// original. Rename publishes each file atomically; a failed rename rolls back
// earlier renames. There is no portable multi-file crash-atomic transaction in
// the stdlib. On rollback failure the error identifies the retained backup.
func commitMetadata(edits []metadataEdit) (err error) {
	defer func() {
		for _, edit := range edits {
			for _, temp := range []string{edit.staged, edit.backup} {
				if temp != "" {
					err = errors.Join(err, os.Remove(temp))
				}
			}
		}
	}()
	for i := range edits {
		edit := &edits[i]
		if edit.staged != "" && edit.backup != "" {
			continue
		}
		edit.staged, err = stageMetadata(edit.name, edit.after, edit.mode)
		if err != nil {
			return err
		}
		edit.backup, err = stageMetadata(edit.name, edit.before, edit.mode)
		if err != nil {
			return err
		}
	}
	for i := range edits {
		if err = os.Rename(edits[i].staged, edits[i].name); err != nil {
			for j := i - 1; j >= 0; j-- {
				if rollbackErr := os.Rename(edits[j].backup, edits[j].name); rollbackErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback failed; recover from %s: %w", edits[j].backup, rollbackErr))
				}
				// On failure keep the backup for manual recovery.
				edits[j].backup = ""
			}
			return err
		}
		edits[i].staged = ""
	}
	return nil
}

const maxMetadataJSONBytes int64 = 64 << 20

var metadataHexRun = regexp.MustCompile(`[0-9a-fA-F]{40,}`)

// changedSHA checks every full-length window, including SHAs embedded in longer
// hex strings. It is linear in input size, independent of commit-map size.
func changedSHA(text string, mapping map[string]string) bool {
	for _, span := range metadataHexRun.FindAllStringIndex(text, -1) {
		run := strings.ToLower(text[span[0]:span[1]])
		for _, size := range []int{40, 64} {
			for i := 0; i+size <= len(run); i++ {
				old := run[i : i+size]
				if next, ok := mapping[old]; ok && old != next {
					return true
				}
			}
		}
	}
	return false
}

func unreviewedSHA(text string, mapping map[string]string) bool {
	if changedSHA(text, mapping) {
		return true
	}
	for _, span := range metadataHexRun.FindAllStringIndex(text, -1) {
		token := text[span[0]:span[1]]
		if len(token) == 40 || len(token) == 64 {
			if _, known := mapping[token]; !known {
				return true
			}
		}
	}
	return false
}

// scanNonJSON never buffers attachments: 63 bytes of overlap cover the longest
// supported full SHA even when it straddles read boundaries.
func scanNonJSON(filename string, mapping map[string]string) error {
	f, err := openRegular(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	buffer := make([]byte, 32*1024+63)
	carry := 0
	for {
		n, err := f.Read(buffer[carry:])
		total := carry + n
		if changedSHA(string(buffer[:total]), mapping) {
			return errors.New("unreviewed changed SHA in non-JSON metadata")
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		carry = min(63, total)
		copy(buffer, buffer[total-carry:total])
	}
}

// metadataAction rejects overlapping applicable rules, including a preserve
// subtree and any rule underneath it. arraySteps marks the type of each parent.
func metadataAction(rules []metadataSelector, tokens []string, arraySteps []bool) (string, error) {
	action := ""
	for _, rule := range rules {
		if len(rule.tokens) > len(tokens) || (rule.action != "preserve" && len(rule.tokens) != len(tokens)) {
			continue
		}
		match := true
		for i, token := range rule.tokens {
			if token != tokens[i] && !(token == "*" && arraySteps[i]) {
				match = false
				break
			}
		}
		if match {
			if action != "" {
				return "", errors.New("overlapping metadata rules")
			}
			action = rule.action
		}
	}
	return action, nil
}

func rewriteCommit(value any, mapping map[string]string) (any, int, error) {
	if value == nil {
		return nil, 0, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, 0, errors.New("commit rule requires a string or null")
	}
	// An explicitly reviewed empty commit field is permitted, but abbreviated
	// or unmapped nonempty values never are. Identity entries must be supplied.
	if text == "" {
		return text, 0, nil
	}
	if !fullGitSHA(text) {
		return nil, 0, errors.New("commit rule requires a full lowercase SHA")
	}
	next, ok := mapping[text]
	if !ok {
		return nil, 0, errors.New("commit reference is missing from commit map")
	}
	if next == text {
		return text, 0, nil
	}
	return next, 1, nil
}

func rewriteCommitURL(value any, mapping map[string]string) (any, int, error) {
	if value == nil || value == "" {
		return value, 0, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, 0, errors.New("commit-url rule requires a string or null")
	}
	u, err := url.Parse(text)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" || u.User != nil {
		return nil, 0, errors.New("commit-url requires an absolute HTTP(S) URL without userinfo")
	}
	// Operate on the ORIGINAL escaped path, not url.String(), so query order,
	// escape spelling, fragments, and all unrelated bytes are preserved exactly.
	start := strings.Index(text, "://") + 3
	pathStart := start + strings.IndexAny(text[start:], "/?#")
	if pathStart < start || text[pathStart] != '/' {
		return nil, 0, errors.New("commit-url must contain a SHA path segment")
	}
	pathEnd := len(text)
	if end := strings.IndexAny(text[pathStart:], "?#"); end >= 0 {
		pathEnd = pathStart + end
	}
	outside, err := url.QueryUnescape(text[:pathStart] + text[pathEnd:])
	if err != nil || unreviewedSHA(outside, mapping) {
		return nil, 0, errors.New("unreviewed SHA outside URL path")
	}
	segments := strings.Split(text[pathStart:pathEnd], "/")
	changed, found := 0, false
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return nil, 0, errors.New("invalid URL path escape")
		}
		if fullGitSHA(decoded) {
			found = true
			next, n, err := rewriteCommit(decoded, mapping)
			if err != nil {
				return nil, 0, err
			}
			if n != 0 {
				segments[i] = next.(string)
				changed += n
			}
		} else if unreviewedSHA(decoded, mapping) {
			return nil, 0, errors.New("unreviewed SHA within URL path segment")
		}
	}
	if !found {
		return nil, 0, errors.New("commit-url must contain a full mapped SHA path segment")
	}
	return text[:pathStart] + strings.Join(segments, "/") + text[pathEnd:], changed, nil
}

func transformMetadata(value any, rules []metadataSelector, mapping map[string]string) (any, int, error) {
	changed := 0
	var walk func(any, []string, []bool, string) (any, error)
	walk = func(value any, tokens []string, arrays []bool, key string) (any, error) {
		action, err := metadataAction(rules, tokens, arrays)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", metadataPointer(tokens), err)
		}
		if action != "preserve" && unreviewedSHA(key, mapping) {
			return nil, errors.New("unreviewed SHA-bearing JSON object key")
		}
		if action == "commit" || action == "commit-url" {
			var next any
			var count int
			if action == "commit" {
				next, count, err = rewriteCommit(value, mapping)
			} else {
				next, count, err = rewriteCommitURL(value, mapping)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", metadataPointer(tokens), err)
			}
			changed += count
			return next, nil
		}
		switch item := value.(type) {
		case string:
			if action != "preserve" && unreviewedSHA(item, mapping) {
				return nil, fmt.Errorf("unreviewed SHA reference at %s", metadataPointer(tokens))
			}
		case json.Number:
			if action != "preserve" && unreviewedSHA(string(item), mapping) {
				return nil, fmt.Errorf("unreviewed SHA-like number at %s", metadataPointer(tokens))
			}
		case []any:
			for i := range item {
				next, err := walk(item[i], append(tokens, strconv.Itoa(i)), append(arrays, true), "")
				if err != nil {
					return nil, err
				}
				item[i] = next
			}
		case map[string]any:
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next, err := walk(item[key], append(tokens, key), append(arrays, false), key)
				if err != nil {
					return nil, err
				}
				item[key] = next
			}
		}
		return value, nil
	}
	result, err := walk(value, nil, nil, "")
	return result, changed, err
}

func readMetadataJSON(filename string) ([]byte, any, error) {
	f, err := openRegular(filename)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > maxMetadataJSONBytes {
		return nil, nil, errors.New("JSON exceeds 64 MiB limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMetadataJSONBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > maxMetadataJSONBytes {
		return nil, nil, errors.New("JSON exceeds 64 MiB limit")
	}
	if !utf8.Valid(data) {
		return nil, nil, errors.New("invalid UTF-8 JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := decodeMetadataValue(dec, 0)
	if err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("multiple JSON documents or trailing data")
	}
	return data, value, nil
}

// RewriteMetadata validates ALL metadata before publishing any replacement.
// It returns the count of replaced commit values / URL segments, zero on error.
// Unknown JSON files, changed SHA substrings, and unmapped full SHA-like tokens
// require reviewed rules; a preserve rule can approve an entire subtree. commit
// rules accept null/empty strings, but all nonempty values need full map entries,
// including identities for unchanged original reachable commits. Re-running with
// an old-only map is intentionally not supported: new SHAs are not original IDs.
// Non-JSON attachments are scanned in bounded chunks for changed old SHA text.
// Git internals are left to the Git rewriter, not treated as metadata. Multiple
// repositories are rejected because the commit map is repository-specific.
// JSON is limited to 64 MiB per file and 512 nesting levels; duplicate keys,
// JSONL/NDJSON and unsupported layouts fail closed. Temporary replacement/backup
// files bound memory to one JSON document, not all metadata files combined.
// The tree must be disposable and exclusively owned. Publication has rollback
// on I/O failure but is not a multi-file crash-atomic transaction.
func RewriteMetadata(root string, policy MetadataPolicy, commitMap map[string]string) (count int, err error) {
	rules, err := compileMetadataPolicy(policy)
	if err != nil {
		return 0, err
	}
	for old, next := range commitMap {
		if !fullGitSHA(old) || !fullGitSHA(next) || len(old) != len(next) {
			return 0, errors.New("commit map requires full lowercase Git SHAs of matching lengths")
		}
	}
	files, err := archiveTree(root)
	if err != nil {
		return 0, err
	}
	if err := validateRepositoryLayout(root, files); err != nil {
		return 0, err
	}
	repos := reposInTree(files)
	if len(repos) > 1 {
		return 0, errors.New("metadata rewriting supports at most one bare repository")
	}
	var names []string
	for rel, info := range files {
		if info.IsDir() || (len(repos) == 1 && pathWithin(rel, repos[0])) {
			continue
		}
		names = append(names, rel)
	}
	sort.Strings(names)
	var edits []metadataEdit
	publishing := false
	defer func() {
		if !publishing {
			for _, edit := range edits {
				for _, temp := range []string{edit.staged, edit.backup} {
					if temp != "" {
						err = errors.Join(err, os.Remove(temp))
					}
				}
			}
		}
	}()
	changed := 0
	for _, rel := range names {
		filename := filepath.Join(root, filepath.FromSlash(rel))
		switch strings.ToLower(path.Ext(rel)) {
		case ".jsonl", ".ndjson":
			return 0, fmt.Errorf("unsupported JSONL/NDJSON metadata: %s", rel)
		case ".json":
			selectors, reviewed := rules[rel]
			if !reviewed {
				return 0, fmt.Errorf("JSON file has no reviewed policy: %s", rel)
			}
			data, value, err := readMetadataJSON(filename)
			if err != nil {
				return 0, fmt.Errorf("invalid JSON in %s: %w", rel, err)
			}
			value, n, err := transformMetadata(value, selectors, commitMap)
			if err != nil {
				return 0, fmt.Errorf("metadata validation in %s: %w", rel, err)
			}
			if n == 0 {
				continue
			}
			after, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				return 0, err
			}
			edits = append(edits, metadataEdit{name: filename, mode: files[rel].Mode()})
			edit := &edits[len(edits)-1]
			edit.staged, err = stageMetadata(filename, append(after, '\n'), edit.mode)
			if err != nil {
				return 0, err
			}
			edit.backup, err = stageMetadata(filename, data, edit.mode)
			if err != nil {
				return 0, err
			}
			changed += n
		default:
			if err := scanNonJSON(filename, commitMap); err != nil {
				return 0, fmt.Errorf("metadata validation in %s: %w", rel, err)
			}
		}
	}
	publishing = true
	if err := commitMetadata(edits); err != nil {
		return 0, err
	}
	return changed, nil
}
