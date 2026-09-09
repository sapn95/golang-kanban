package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The credentials AWS uses in its own signing examples. They are not a secret
// and they are not valid anywhere.
const (
	testKeyID  = "AKIDEXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

var signedAt = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func testS3(t *testing.T, endpoint string) *S3 {
	t.Helper()
	return &S3{
		Bucket:          "kanban-backups",
		Prefix:          "pi/",
		Region:          "eu-central-2",
		Endpoint:        endpoint,
		AccessKeyID:     testKeyID,
		SecretAccessKey: testSecret,
		Now:             func() time.Time { return signedAt },
	}
}

// TestEmptyPayloadHash pins the constant the signature uses for a request with
// no body. It is written out in the source, and a typo in it would only show up
// as a rejected request against a real bucket.
func TestEmptyPayloadHash(t *testing.T) {
	sum := sha256.Sum256(nil)
	if got := hex.EncodeToString(sum[:]); got != emptyPayload {
		t.Errorf("emptyPayload = %s, want %s", emptyPayload, got)
	}
}

// TestSignatureMatchesBotocore is the check that matters for hand-rolled
// SigV4: the header this package produces is the header the AWS SDK produces
// for the same request. The expected values were taken from botocore 1.43.89:
//
//	auth.get_current_datetime = lambda: WHEN
//	req = AWSRequest(method=..., url=..., data=..., headers=...)
//	SigV4Auth(Credentials(key, secret), "s3", "eu-central-2").add_auth(req)
//
// The same harness reproduces the signature AWS publishes for its get-vanilla
// test case, which is what says the harness itself was right.
func TestSignatureMatchesBotocore(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"format":1}`)

	t.Run("put", func(t *testing.T) {
		s := testS3(t, "")
		req, err := s.request(ctx, http.MethodPut, s.key("kanban-20260304T050607Z.json"), nil, body)
		must(t, "build request", err)
		// Put sets the type after building the request, and it is signed, so the
		// signature has to be taken again over the final header set.
		req.Header.Set("Content-Type", "application/json")
		must(t, "sign", s.sign(req, body))

		if got := req.URL.String(); got != "https://kanban-backups.s3.eu-central-2.amazonaws.com/pi/kanban-20260304T050607Z.json" {
			t.Errorf("URL = %s", got)
		}
		wantSum := "3f0a99256beeb89a6b9f2793885291a6454928fd74903159ab2e51452b13df6f"
		if got := req.Header.Get("X-Amz-Content-Sha256"); got != wantSum {
			t.Errorf("payload hash = %s, want %s", got, wantSum)
		}
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260304/eu-central-2/s3/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, " +
			"Signature=242428f219774d4d73ebbb5a11e1420b5754092de7e1cd512979080b53808472"
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("list", func(t *testing.T) {
		s := testS3(t, "")
		req, err := s.request(ctx, http.MethodGet, "", url.Values{"list-type": {"2"}, "prefix": {"pi/"}}, nil)
		must(t, "build request", err)
		if got := req.URL.String(); got != "https://kanban-backups.s3.eu-central-2.amazonaws.com/?list-type=2&prefix=pi%2F" {
			t.Errorf("URL = %s", got)
		}
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260304/eu-central-2/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
			"Signature=067d87373a6a8eaab4ff63e9d8046c808f711e3bbf40ecffc01ebf26d6d8f65b"
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
		}
	})

	// Temporary credentials, where the token is signed as a header. A signature
	// that left it out is accepted by nothing and is easy to miss, because the
	// permanent-credential path keeps working.
	t.Run("delete with a session token", func(t *testing.T) {
		s := testS3(t, "")
		s.SessionToken = "SESSIONTOKEN/with+chars="
		req, err := s.request(ctx, http.MethodDelete, s.key("kanban-20260304T050607Z.json"), nil, nil)
		must(t, "build request", err)
		if got := req.Header.Get("X-Amz-Security-Token"); got != s.SessionToken {
			t.Errorf("token header = %q", got)
		}
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260304/eu-central-2/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, " +
			"Signature=98fc6fde8f07464a31d1a9d4fd48e728290206171bfbfc64547008386794f5e9"
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
		}
	})
}

// TestSigningKeyMatchesTheAWSTestCase signs the string AWS publishes for its
// get-vanilla case with a key derived for that case's scope. It anchors the
// derivation chain, which is the part of SigV4 that is easy to get subtly
// wrong, to a vendor-published number.
func TestSigningKeyMatchesTheAWSTestCase(t *testing.T) {
	const toSign = "AWS4-HMAC-SHA256\n" +
		"20150830T123600Z\n" +
		"20150830/us-east-1/service/aws4_request\n" +
		"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"
	key := hmacSHA256([]byte("AWS4"+testSecret), "20150830")
	key = hmacSHA256(key, "us-east-1")
	key = hmacSHA256(key, "service")
	key = hmacSHA256(key, "aws4_request")
	const want = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := hex.EncodeToString(hmacSHA256(key, toSign)); got != want {
		t.Errorf("signature = %s, want AWS's %s", got, want)
	}
}

// TestCanonicalHeadersAreTheOnesS3ActsOn: host ties the signature to a bucket,
// the x-amz-* headers change what the request does, and a header the client
// adds on the way out must not be signed or every request would fail.
func TestCanonicalHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "https://b.s3.eu-central-2.amazonaws.com/k", nil)
	must(t, "request", err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Date", "20260304T050607Z")
	req.Header.Set("X-Amz-Meta-Note", "  two   spaces  ")
	req.Header.Set("User-Agent", "kanban")
	req.Header.Set("Accept-Encoding", "gzip")

	block, signed := canonicalHeaders(req)
	if signed != "content-type;host;x-amz-date;x-amz-meta-note" {
		t.Errorf("signed headers = %q", signed)
	}
	if !strings.Contains(block, "host:b.s3.eu-central-2.amazonaws.com\n") {
		t.Errorf("block = %q", block)
	}
	// Runs of whitespace in a value collapse, which is what the rules ask for.
	if !strings.Contains(block, "x-amz-meta-note:two spaces\n") {
		t.Errorf("block did not fold the whitespace: %q", block)
	}
	if strings.Contains(block, "user-agent") || strings.Contains(block, "accept-encoding") {
		t.Errorf("block signed a header the transport may change: %q", block)
	}
}

func TestCanonicalPathAndQuery(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/pi/kanban-20260304T050607Z.json", "/pi/kanban-20260304T050607Z.json"},
		{"/a b/c+d", "/a%20b/c%2Bd"},
		{"/~tilde-and.dot_", "/~tilde-and.dot_"},
	} {
		if got := canonicalPath(&url.URL{Path: tc.in}); got != tc.want {
			t.Errorf("canonicalPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := canonicalQuery(nil); got != "" {
		t.Errorf("canonicalQuery(nil) = %q", got)
	}
	// Sorted by name, then by value, and a space is %20 rather than the plus
	// net/url would write.
	got := canonicalQuery(url.Values{"prefix": {"pi/"}, "list-type": {"2"}, "x": {"b", "a b"}})
	if want := "list-type=2&prefix=pi%2F&x=a%20b&x=b"; got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
}

func TestS3StringHidesTheCredentials(t *testing.T) {
	s := testS3(t, "https://minio.example.com")
	got := s.String()
	if !strings.Contains(got, "s3://kanban-backups/pi/") || !strings.Contains(got, "minio.example.com") {
		t.Errorf("String = %q", got)
	}
	if strings.Contains(got, testSecret) || strings.Contains(got, testKeyID) {
		t.Errorf("String leaks credentials: %q", got)
	}
}

func TestS3RefusesToSignWithoutCredentials(t *testing.T) {
	s := &S3{Bucket: "b", Region: "eu-central-2"}
	if err := s.Put(context.Background(), Name(stamp), []byte("{}")); err == nil ||
		!strings.Contains(err.Error(), "no credentials") {
		t.Errorf("Put = %v, want a complaint about credentials", err)
	}
}

func TestS3EndpointStyles(t *testing.T) {
	ctx := context.Background()
	// A set endpoint is addressed path-style, so the bucket is a path segment
	// and no wildcard certificate is needed.
	s := testS3(t, "https://minio.example.com:9000")
	req, err := s.request(ctx, http.MethodPut, s.key("kanban-20260304T050607Z.json"), nil, []byte("{}"))
	must(t, "request", err)
	if got := req.URL.String(); got != "https://minio.example.com:9000/kanban-backups/pi/kanban-20260304T050607Z.json" {
		t.Errorf("URL = %s", got)
	}
	if req.Host != "minio.example.com:9000" && req.URL.Host != "minio.example.com:9000" {
		t.Errorf("Host = %q", req.URL.Host)
	}
	// A list against a path-style endpoint asks about the bucket, not the root.
	req, err = s.request(ctx, http.MethodGet, "", url.Values{"list-type": {"2"}}, nil)
	must(t, "request", err)
	if got := req.URL.String(); got != "https://minio.example.com:9000/kanban-backups?list-type=2" {
		t.Errorf("list URL = %s", got)
	}

	// An endpoint under a path of its own, as a gateway that puts the whole of S3
	// behind one prefix does. The prefix is part of the request and therefore
	// part of the signature, so losing it would sign for another resource.
	s = testS3(t, "https://gateway.example.com/s3/")
	req, err = s.request(ctx, http.MethodPut, s.key("kanban-20260304T050607Z.json"), nil, []byte("{}"))
	must(t, "request", err)
	if got := req.URL.String(); got != "https://gateway.example.com/s3/kanban-backups/pi/kanban-20260304T050607Z.json" {
		t.Errorf("URL = %s", got)
	}
	if got, want := canonicalPath(req.URL), "/s3/kanban-backups/pi/kanban-20260304T050607Z.json"; got != want {
		t.Errorf("signed path = %s, want %s", got, want)
	}
	req, err = s.request(ctx, http.MethodGet, "", url.Values{"list-type": {"2"}}, nil)
	must(t, "request", err)
	if got := req.URL.String(); got != "https://gateway.example.com/s3/kanban-backups?list-type=2" {
		t.Errorf("list URL = %s", got)
	}

	for _, bad := range []string{"://nope", "not-a-url"} {
		s := testS3(t, bad)
		if _, err := s.endpoint("k", nil); err == nil {
			t.Errorf("endpoint %q accepted", bad)
		}
	}
}

// s3Server is a bucket good enough for the four calls this package makes.
type s3Server struct {
	objects map[string][]byte
	// pages > 1 makes List answer with continuation tokens, which is the path a
	// bucket with more than a thousand keys takes.
	pages    int
	requests []*http.Request
	fail     int
	failBody string
}

func (b *s3Server) handler(t *testing.T) http.Handler {
	t.Helper()
	if b.objects == nil {
		b.objects = map[string][]byte{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		b.requests = append(b.requests, r)
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, algorithm+" Credential="+testKeyID) {
			t.Errorf("%s %s: Authorization = %q", r.Method, r.URL, auth)
		}
		// Every request carries the payload hash it signed.
		sum := sha256.Sum256(body)
		if got, want := r.Header.Get("X-Amz-Content-Sha256"), hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s %s: payload hash = %s, want %s", r.Method, r.URL, got, want)
		}
		if b.fail != 0 {
			w.WriteHeader(b.fail)
			_, _ = w.Write([]byte(b.failBody))
			return
		}
		switch r.Method {
		case http.MethodGet:
			b.list(w, r)
		case http.MethodPut:
			b.objects[strings.TrimPrefix(r.URL.Path, "/kanban-backups/")] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(b.objects, strings.TrimPrefix(r.URL.Path, "/kanban-backups/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// list answers one page per key, so the continuation is exercised with two
// objects instead of a thousand.
func (b *s3Server) list(w http.ResponseWriter, r *http.Request) {
	if got := r.URL.Query().Get("list-type"); got != "2" {
		http.Error(w, "want ListObjectsV2", http.StatusBadRequest)
		return
	}
	var keys []string
	for key := range b.objects {
		if strings.HasPrefix(key, r.URL.Query().Get("prefix")) {
			keys = append(keys, key)
		}
	}
	sortNames(keys)
	page, truncated := keys, false
	if b.pages > 1 && len(keys) > 0 {
		from := 0
		if token := r.URL.Query().Get("continuation-token"); token != "" {
			for i, key := range keys {
				if key == token {
					from = i
				}
			}
		}
		page, truncated = keys[from:from+1], from+1 < len(keys)
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	fmt.Fprintf(&sb, `<IsTruncated>%t</IsTruncated>`, truncated)
	if truncated {
		next := ""
		for i, key := range keys {
			if key == page[0] && i+1 < len(keys) {
				next = keys[i+1]
			}
		}
		fmt.Fprintf(&sb, `<NextContinuationToken>%s</NextContinuationToken>`, next)
	}
	for _, key := range page {
		fmt.Fprintf(&sb, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, key, len(b.objects[key]))
	}
	sb.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(sb.String()))
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func TestS3PutListDelete(t *testing.T) {
	ctx := context.Background()
	bucket := &s3Server{}
	srv := httptest.NewServer(bucket.handler(t))
	defer srv.Close()
	s := testS3(t, srv.URL)
	s.Client = srv.Client()

	names, err := s.List(ctx)
	must(t, "list an empty bucket", err)
	if len(names) != 0 {
		t.Errorf("List = %v", names)
	}

	first := Name(stamp.Add(-24 * time.Hour))
	second := Name(stamp)
	must(t, "put", s.Put(ctx, second, []byte(`{"format":1}`)))
	must(t, "put", s.Put(ctx, first, []byte(`{"format":1}`)))
	// The prefix is in the key and nowhere above the interface.
	if _, ok := bucket.objects["pi/"+first]; !ok {
		t.Errorf("bucket holds %v, want the prefixed key", bucket.objects)
	}

	names, err = s.List(ctx)
	must(t, "list", err)
	if len(names) != 2 || names[0] != first || names[1] != second {
		t.Errorf("List = %v, want %v", names, []string{first, second})
	}

	must(t, "delete", s.Delete(ctx, first))
	names, err = s.List(ctx)
	must(t, "list", err)
	if len(names) != 1 || names[0] != second {
		t.Errorf("List = %v, want only %s", names, second)
	}
}

// TestS3ListPaginatesAndIgnoresOtherKeys covers the two things that make a
// listing more than one request: a truncated page, and a bucket that holds
// somebody else's files.
func TestS3ListPaginatesAndIgnoresOtherKeys(t *testing.T) {
	ctx := context.Background()
	bucket := &s3Server{pages: 2, objects: map[string][]byte{
		"pi/" + Name(stamp.Add(-48*time.Hour)): []byte("{}"),
		"pi/" + Name(stamp.Add(-24*time.Hour)): []byte("{}"),
		"pi/" + Name(stamp):                    []byte("{}"),
		"pi/notes.txt":                         []byte("mine"),
		"other/" + Name(stamp):                 []byte("{}"),
	}}
	srv := httptest.NewServer(bucket.handler(t))
	defer srv.Close()
	s := testS3(t, srv.URL)
	s.Client = srv.Client()

	names, err := s.List(ctx)
	must(t, "list", err)
	if len(names) != 3 {
		t.Fatalf("List = %v, want the three snapshots under the prefix", names)
	}
	for _, name := range names {
		if _, ok := TakenAt(name); !ok {
			t.Errorf("List returned %q, which is not a snapshot name", name)
		}
	}
	if len(bucket.requests) < 3 {
		t.Errorf("%d requests, want one per page", len(bucket.requests))
	}
}

// TestS3ListStopsWalking is the guard on a bucket that is not only ours: a
// scheduler that walked a listing forever would stop taking backups.
func TestS3ListStopsWalking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated>` +
			`<NextContinuationToken>more</NextContinuationToken></ListBucketResult>`))
	}))
	defer srv.Close()
	s := testS3(t, srv.URL)
	s.Client = srv.Client()
	_, err := s.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "more than 50 pages") {
		t.Errorf("List = %v, want it to give up", err)
	}
}

func TestS3ReportsWhatTheBucketSaid(t *testing.T) {
	ctx := context.Background()
	t.Run("an error document on one line", func(t *testing.T) {
		bucket := &s3Server{fail: http.StatusForbidden, failBody: "<Error>\n  <Code>AccessDenied</Code>\n" +
			"  <Message>Access Denied</Message>\n</Error>\n"}
		srv := httptest.NewServer(bucket.handler(t))
		defer srv.Close()
		s := testS3(t, srv.URL)
		s.Client = srv.Client()
		err := s.Put(ctx, Name(stamp), []byte("{}"))
		if err == nil {
			t.Fatal("Put succeeded against a 403")
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("error spans lines: %q", err)
		}
		if !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "403") {
			t.Errorf("error = %v, want the status and the code", err)
		}
	})

	t.Run("a proxy error page is cut short", func(t *testing.T) {
		bucket := &s3Server{fail: http.StatusBadGateway, failBody: strings.Repeat("x", 8<<10)}
		srv := httptest.NewServer(bucket.handler(t))
		defer srv.Close()
		s := testS3(t, srv.URL)
		s.Client = srv.Client()
		err := s.Delete(ctx, Name(stamp))
		if err == nil {
			t.Fatal("Delete succeeded against a 502")
		}
		if len(err.Error()) > maxErrorBody+300 {
			t.Errorf("error is %d bytes; the whole page was quoted", len(err.Error()))
		}
	})

	t.Run("a bucket that does not answer", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		s := testS3(t, srv.URL)
		s.Client = srv.Client()
		srv.Close()
		err := s.Put(ctx, Name(stamp), []byte("{}"))
		if err == nil || !strings.Contains(err.Error(), "PUT s3://kanban-backups") {
			t.Errorf("error = %v, want it to name the call and the target", err)
		}
	})

	t.Run("xml that is not a listing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not xml at all"))
		}))
		defer srv.Close()
		s := testS3(t, srv.URL)
		s.Client = srv.Client()
		if _, err := s.List(ctx); err == nil {
			t.Error("List accepted a response that is not a listing")
		}
	})
}

// TestS3DefaultClientHasATimeout: a scheduler blocked on a socket stops taking
// backups without saying anything, which is worse than a failed one.
func TestS3DefaultClientHasATimeout(t *testing.T) {
	s := &S3{}
	if s.client().Timeout == 0 {
		t.Error("the default client would wait forever")
	}
	if s.now().IsZero() {
		t.Error("now() must fall back to the clock")
	}
}
