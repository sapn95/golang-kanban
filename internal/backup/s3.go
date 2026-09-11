package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 keeps snapshots in an S3 bucket, or in anything that speaks enough of the
// same protocol: MinIO, Garage, Ceph, Backblaze B2 through its S3 endpoint.
//
// It signs its own requests rather than pulling in the AWS SDK, which is around
// forty modules for four calls: PUT one object, list them, delete the old ones.
// Signature Version 4 is a documented, stable algorithm and the whole of it is
// sign below. See [0012] for the trade this makes.
//
// [0012]: ../../docs/adr/0012-snapshots-are-the-portable-format.md
type S3 struct {
	Bucket string
	// Prefix is put in front of every name, so one bucket can hold the
	// snapshots of several deployments. It is stripped again on the way out, so
	// nothing above Target sees it.
	Prefix string
	Region string
	// Endpoint is empty for AWS, where the bucket is a subdomain. Anything else
	// is treated as an S3-compatible server and addressed path-style, which is
	// what MinIO and the rest expect and what avoids needing a wildcard
	// certificate for a bucket name.
	Endpoint string

	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is set when the credentials are temporary, from a role or
	// from AWS SSO. It is signed as a header, not as a query parameter.
	SessionToken string

	// Client is optional; the default has a timeout, because a scheduler that
	// blocks forever on a socket stops taking backups without saying so.
	Client *http.Client
	// Now is optional; tests replace it to get a stable signature.
	Now func() time.Time
}

var _ Target = (*S3)(nil)

// String is what the log says about the target. Neither the key nor the secret
// appear in it.
func (s *S3) String() string {
	where := "s3://" + s.Bucket + "/" + s.Prefix
	if s.Endpoint != "" {
		where += " at " + s.Endpoint
	}
	return where
}

// client is the HTTP client for S3, defaulting to one with a timeout: a backup
// that hangs forever is a backup that never fails and never happens.
func (s *S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// now is the clock the signature is dated with, injectable so a test can sign
// against a known timestamp.
func (s *S3) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// key is the object key of a snapshot name.
func (s *S3) key(name string) string { return s.Prefix + name }

// Put writes one snapshot.
func (s *S3) Put(ctx context.Context, name string, body []byte) error {
	req, err := s.request(ctx, http.MethodPut, s.key(name), nil, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = s.do(req)
	return err
}

// Delete removes one snapshot. S3 answers 204 whether or not the key was there,
// which is the semantics Target wants anyway.
func (s *S3) Delete(ctx context.Context, name string) error {
	req, err := s.request(ctx, http.MethodDelete, s.key(name), nil, nil)
	if err != nil {
		return err
	}
	_, err = s.do(req)
	return err
}

// listResult is as much of ListObjectsV2 as this package reads.
type listResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextToken   string   `xml:"NextContinuationToken"`
	Contents    []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// List returns the snapshot names in the bucket, oldest first. A key that is
// not a snapshot name is left out, so pointing the prefix at a directory that
// holds other things is safe and retention will not touch them.
func (s *S3) List(ctx context.Context) ([]string, error) {
	var names []string
	token := ""
	for page := 0; ; page++ {
		query := url.Values{"list-type": {"2"}}
		if s.Prefix != "" {
			query.Set("prefix", s.Prefix)
		}
		if token != "" {
			query.Set("continuation-token", token)
		}
		req, err := s.request(ctx, http.MethodGet, "", query, nil)
		if err != nil {
			return nil, err
		}
		body, err := s.do(req)
		if err != nil {
			return nil, err
		}
		var res listResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("list %s: %w", s, err)
		}
		for _, c := range res.Contents {
			name := strings.TrimPrefix(c.Key, s.Prefix)
			if _, ok := TakenAt(name); ok {
				names = append(names, name)
			}
		}
		if !res.IsTruncated || res.NextToken == "" {
			break
		}
		token = res.NextToken
		// A bucket answers 1000 keys a page. Retention keeps a handful, so
		// anything past this is a bucket that is not only ours, and walking it
		// forever would hold the scheduler up on every tick.
		if page >= 50 {
			return nil, fmt.Errorf("list %s: more than %d pages", s, page)
		}
	}
	sortNames(names)
	return names, nil
}

// endpoint builds the URL of one key and reports the Host to sign.
func (s *S3) endpoint(key string, query url.Values) (*url.URL, error) {
	base := s.Endpoint
	if base == "" {
		base = "https://" + s.Bucket + ".s3." + s.Region + ".amazonaws.com"
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: %w", base, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("endpoint %q: no host", base)
	}
	// An endpoint may carry a path of its own, as a gateway that puts the whole
	// of S3 under one prefix does. Dropping it would sign a request for a
	// resource the operator did not name.
	path := strings.TrimSuffix(u.Path, "/") + "/"
	if s.Endpoint != "" {
		// Path-style: the bucket is the first segment after that prefix.
		path += s.Bucket + "/"
	}
	u.Path = strings.TrimSuffix(path+key, "/")
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = canonicalQuery(query)
	return u, nil
}

// request builds a signed request for one object. The address comes from
// endpoint, which is virtual-host style against AWS and path-style only when an
// endpoint is configured, so MinIO and the rest work without DNS per bucket.
func (s *S3) request(ctx context.Context, method, key string, query url.Values, body []byte) (*http.Request, error) {
	u, err := s.endpoint(key, query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	if err := s.sign(req, body); err != nil {
		return nil, err
	}
	return req, nil
}

// maxErrorBody is how much of a failure response is quoted back. S3 says what
// went wrong in a short XML document; anything longer is a proxy's error page.
const maxErrorBody = 2 << 10

// do sends a request and reads the body, turning a non-2xx into an error that
// carries what S3 said rather than just the status.
func (s *S3) do(req *http.Request) ([]byte, error) {
	res, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, s, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
		return nil, fmt.Errorf("%s %s: %s: %s", req.Method, s, res.Status,
			strings.TrimSpace(collapse(string(snippet))))
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", req.Method, s, err)
	}
	return body, nil
}

// collapse puts an XML error document on one line, so it fits a log line.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// --- Signature Version 4 -----------------------------------------------------
//
// The algorithm is: build a canonical form of the request, hash it, sign the
// hash with a key derived from date, region and service, and put the result in
// an Authorization header. It is written out here rather than pulled in because
// it is eighty lines and it does not change.

const (
	algorithm = "AWS4-HMAC-SHA256"
	// s3Service is the service name in the credential scope. Not called
	// service, which is a package this repository has.
	s3Service    = "s3"
	amzDateAttr  = "20060102T150405Z"
	dateStampFmt = "20060102"
	// emptyPayload is sha256(""), which is what S3 wants in
	// x-amz-content-sha256 for a request with no body.
	emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// sign adds an AWS Signature Version 4 header. Written out here rather than
// pulled in with the SDK: this is the only AWS call the process makes, and the
// SDK is larger than the whole binary.
func (s *S3) sign(req *http.Request, body []byte) error {
	if s.AccessKeyID == "" || s.SecretAccessKey == "" {
		return fmt.Errorf("s3 %s: no credentials", s.Bucket)
	}
	now := s.now()
	amzDate := now.Format(amzDateAttr)
	dateStamp := now.Format(dateStampFmt)

	payload := emptyPayload
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		payload = hex.EncodeToString(sum[:])
	}
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("X-Amz-Date", amzDate)
	if s.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.SessionToken)
	}

	headers, signed := canonicalHeaders(req)
	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL),
		req.URL.RawQuery,
		headers,
		signed,
		payload,
	}, "\n")

	scope := dateStamp + "/" + s.Region + "/" + s3Service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{algorithm, amzDate, scope, hex.EncodeToString(sum[:])}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.SecretAccessKey), dateStamp)
	key = hmacSHA256(key, s.Region)
	key = hmacSHA256(key, s3Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, s.AccessKeyID, scope, signed, signature))
	return nil
}

// hmacSHA256 is one round of the signing key derivation.
func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalHeaders returns the header block and the list of names it covers.
// Host is signed because it is what ties a signature to one bucket, and every
// x-amz-* header is signed because S3 acts on them.
func canonicalHeaders(req *http.Request) (block, signed string) {
	values := map[string]string{"host": req.Host}
	if values["host"] == "" {
		values["host"] = req.URL.Host
	}
	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		if lower != "content-type" && !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		values[lower] = strings.Join(vs, ",")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(strings.Fields(values[name]), " "))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalPath is the path as S3 wants it signed: encoded once, with the
// slashes left alone, and never empty.
func canonicalPath(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	segments := strings.Split(u.Path, "/")
	for i, seg := range segments {
		segments[i] = uriEncode(seg)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery encodes the query the way the signature needs it: sorted by
// name, every character escaped except the unreserved ones. net/url differs
// from that in one place, a space, which it writes as a plus.
func canonicalQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		values := append([]string(nil), query[name]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, uriEncode(name)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is RFC 3986 percent-encoding of everything but the unreserved
// characters, which is what the signing rules ask for.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
