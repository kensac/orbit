package sources

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

const (
	lakeListBytes = 1 << 20
	lakeService   = "s3"
)

// lake fetches a bounded, lexically ordered set of gzip NDJSON objects from
// an S3-compatible bucket. The output is a JSON array for existing transforms.
func (f *Fetcher) lake(ctx context.Context, s spec.Source, vars Vars) (*Fetched, error) {
	if s.MaxBytes <= 0 {
		return nil, fmt.Errorf("lake max_bytes must be positive")
	}
	endpoint, err := url.Parse(s.URL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("lake url must be an http(s) S3 endpoint")
	}
	used := map[string]bool{}
	prefix, err := f.render(s.Prefix, vars, used)
	if err != nil {
		return nil, fmt.Errorf("prefix: %w", err)
	}
	access, err := f.render(s.AccessKeyID, vars, used)
	if err != nil {
		return nil, fmt.Errorf("access_key_id: %w", err)
	}
	secret, err := f.render(s.SecretAccessKey, vars, used)
	if err != nil {
		return nil, fmt.Errorf("secret_access_key: %w", err)
	}
	token := ""
	if s.SessionToken != "" {
		token, err = f.render(s.SessionToken, vars, used)
		if err != nil {
			return nil, fmt.Errorf("session_token: %w", err)
		}
	}
	if access == "" || secret == "" {
		return nil, fmt.Errorf("lake credentials are empty")
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "." || part == ".." {
			return nil, fmt.Errorf("lake prefix may not contain a dot path segment")
		}
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "\\") {
		return nil, fmt.Errorf("lake prefix must be a relative object-key prefix")
	}
	if err := f.allow(used, endpoint.String()+"/"); err != nil {
		return nil, err
	}
	maxObjects := s.MaxObjects
	if maxObjects == 0 {
		maxObjects = spec.DefaultLakeObjects
	}
	if maxObjects < 1 || maxObjects > spec.MaxLakeObjects {
		return nil, fmt.Errorf("lake max_objects is at most %d", spec.MaxLakeObjects)
	}
	keys, listedBytes, err := f.lakeKeys(ctx, endpoint, s.Bucket, prefix, s.Region, access, secret, token, maxObjects, s.MaxBytes, used)
	if err != nil {
		return nil, err
	}
	slices.Sort(keys)
	rows := make([]json.RawMessage, 0)
	compressed, decoded := listedBytes, int64(0)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) || strings.HasSuffix(key, "/") || !strings.HasSuffix(key, ".json.gz") {
			return nil, fmt.Errorf("lake object under prefix must be a .json.gz file")
		}
		for _, part := range strings.Split(key, "/") {
			if part == "." || part == ".." {
				return nil, fmt.Errorf("lake object key has a dot path segment")
			}
		}
		remaining := s.MaxBytes - compressed
		if remaining <= 0 {
			return nil, fmt.Errorf("lake objects exceed max_bytes (%d)", s.MaxBytes)
		}
		objURL := lakeObjectURL(endpoint, s.Bucket, key)
		body, err := f.lakeGet(ctx, objURL, s.Region, access, secret, token, remaining, used)
		if err != nil {
			return nil, fmt.Errorf("lake object fetch failed: %w", err)
		}
		compressed += int64(len(body))
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("lake object is not valid gzip: %w", err)
		}
		plain, readErr := readBounded(zr, s.MaxBytes-decoded)
		closeErr := zr.Close()
		if readErr != nil {
			return nil, fmt.Errorf("lake decompressed data exceeds max_bytes (%d): %w", s.MaxBytes, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("lake object gzip close: %w", closeErr)
		}
		decoded += int64(len(plain))
		for _, line := range bytes.Split(plain, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var row map[string]json.RawMessage
			if err := json.Unmarshal(line, &row); err != nil {
				return nil, fmt.Errorf("lake object contains invalid NDJSON: %w", err)
			}
			if row == nil {
				return nil, fmt.Errorf("lake NDJSON records must be JSON objects")
			}
			rows = append(rows, append(json.RawMessage(nil), line...))
		}
	}
	if rows == nil {
		rows = []json.RawMessage{}
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > s.MaxBytes {
		return nil, fmt.Errorf("lake output exceeds max_bytes (%d)", s.MaxBytes)
	}
	return &Fetched{Name: s.Name, MediaType: "application/json", Content: out,
		Meta: map[string]any{"bucket": s.Bucket, "prefix": prefix, "objects": len(keys), "records": len(rows)}}, nil
}

type lakeListResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

func (f *Fetcher) lakeKeys(ctx context.Context, endpoint *url.URL, bucket, prefix, region, access, secret, token string, maxObjects int, maxBytes int64, used map[string]bool) ([]string, int64, error) {
	u := lakeObjectURL(endpoint, bucket, "")
	pageSize := min(maxObjects+1, spec.MaxLakeObjects)
	q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {fmt.Sprint(pageSize)}}
	u.RawQuery = canonicalLakeQuery(q)
	b, err := f.lakeGet(ctx, u, region, access, secret, token, min(maxBytes, lakeListBytes), used)
	if err != nil {
		return nil, 0, fmt.Errorf("lake list failed: %w", err)
	}
	var result lakeListResult
	if err := xml.Unmarshal(b, &result); err != nil {
		return nil, 0, fmt.Errorf("lake list response is invalid XML: %w", err)
	}
	if result.IsTruncated || result.NextContinuationToken != "" || len(result.Contents) > maxObjects {
		return nil, 0, fmt.Errorf("lake prefix has more than max_objects (%d); narrow the prefix or raise max_objects", maxObjects)
	}
	keys := make([]string, len(result.Contents))
	for i, item := range result.Contents {
		keys[i] = item.Key
	}
	return keys, int64(len(b)), nil
}

func (f *Fetcher) lakeGet(ctx context.Context, u *url.URL, region, access, secret, token string, maxBytes int64, used map[string]bool) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("source exceeds max_bytes")
	}
	if err := f.allow(used, u.String()); err != nil {
		return nil, err
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "orbit/1 (+https://github.com/kensac/orbit)")
	signLakeRequest(req, region, access, secret, token, now().UTC())
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("redirect refused for a signed lake request")
	}
	resp, err := clientCopy.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("request returned %s", resp.Status)
	}
	return readBounded(resp.Body, maxBytes)
}

func lakeObjectURL(endpoint *url.URL, bucket, key string) *url.URL {
	u := *endpoint
	escaped := strings.TrimRight(endpoint.EscapedPath(), "/") + "/" + awsEscape(bucket, true)
	if key != "" {
		parts := strings.Split(key, "/")
		for i := range parts {
			parts[i] = awsEscape(parts[i], true)
		}
		escaped += "/" + strings.Join(parts, "/")
	}
	pathValue, _ := url.PathUnescape(escaped)
	u.Path, u.RawPath = pathValue, escaped
	u.RawQuery = ""
	u.ForceQuery = false
	return &u
}

func signLakeRequest(req *http.Request, region, access, secret, token string, now time.Time) {
	date := now.Format("20060102")
	timestamp := now.Format("20060102T150405Z")
	payloadHash := sha256Hex(nil)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", timestamp)
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	}
	canonicalNames := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if token != "" {
		canonicalNames = append(canonicalNames, "x-amz-security-token")
	}
	slices.Sort(canonicalNames)
	var canonicalHeaders strings.Builder
	for _, name := range canonicalNames {
		value := ""
		if name == "host" {
			value = req.URL.Host
		} else {
			value = req.Header.Get(name)
		}
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(value))
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(canonicalNames, ";")
	canonicalRequest := strings.Join([]string{req.Method, req.URL.EscapedPath(), canonicalLakeQuery(req.URL.Query()), canonicalHeaders.String(), signedHeaders, payloadHash}, "\n")
	canonicalHash := sha256Hex([]byte(canonicalRequest))
	scope := date + "/" + region + "/" + lakeService + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + timestamp + "\n" + scope + "\n" + canonicalHash
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, lakeService)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+access+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func canonicalLakeQuery(values url.Values) string {
	pairs := make([]string, 0)
	for key, entries := range values {
		for _, value := range entries {
			pairs = append(pairs, awsEscape(key, true)+"="+awsEscape(value, true))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func awsEscape(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	for _, b := range []byte(s) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-_.~", rune(b)) || b == '/' && !encodeSlash {
			out.WriteByte(b)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexDigits[b>>4])
		out.WriteByte(hexDigits[b&15])
	}
	return out.String()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = io.WriteString(h, value)
	return h.Sum(nil)
}
