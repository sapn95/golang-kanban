package web

import (
	"net/http"
	"strings"
	"testing"
)

// The route is registered as a literal so the endpoint reference can be checked
// against it, and the policy is built from the constant. This is what keeps the
// two from drifting apart into a report nobody receives.
func TestCSPReportPathIsRegistered(t *testing.T) {
	e := seeded(t)
	if rr := e.do(http.MethodPost, cspReportPath, strings.NewReader("{}")); rr.Code != http.StatusNoContent {
		t.Fatalf("POST %s answered %d, want 204; the route and the constant disagree", cspReportPath, rr.Code)
	}
}

func TestCSPReportIsLogged(t *testing.T) {
	const reportURI = `{"csp-report":{"document-uri":"https://board.example/b/demo",` +
		`"violated-directive":"script-src","effective-directive":"script-src-elem",` +
		`"blocked-uri":"https://cdn.example/thing.js","source-file":"https://board.example/b/demo",` +
		`"line-number":42}}`
	const reportTo = `[{"type":"csp-violation","url":"https://board.example/b/demo","body":{` +
		`"documentURL":"https://board.example/b/demo","effectiveDirective":"style-src-attr",` +
		`"blockedURL":"inline","sample":"color: red"}}]`

	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"report-uri, what Firefox and Safari send", reportURI,
			[]string{"script-src-elem", "https://cdn.example/thing.js", "line=42"}},
		{"report-to, what Chrome sends", reportTo,
			[]string{"style-src-attr", "blocked=inline", "color: red"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := seeded(t)
			rr := e.do(http.MethodPost, cspReportPath, strings.NewReader(tc.body))
			if rr.Code != http.StatusNoContent {
				t.Fatalf("answered %d, want 204", rr.Code)
			}
			log := e.log.String()
			if !strings.Contains(log, "content security policy refused something") {
				t.Fatalf("nothing was logged:\n%s", log)
			}
			for _, want := range tc.want {
				if !strings.Contains(log, want) {
					t.Errorf("the log line does not carry %q:\n%s", want, log)
				}
			}
		})
	}
}

// A browser is not waiting for an answer and has nowhere to put an error, so
// nothing it can send is allowed to become a 500 or a 400.
func TestCSPReportSwallowsRubbish(t *testing.T) {
	for _, body := range []string{"", "not json", "[]", "null", `{"csp-report":null}`, `[{"type":"deprecation"}]`} {
		e := seeded(t)
		if rr := e.do(http.MethodPost, cspReportPath, strings.NewReader(body)); rr.Code != http.StatusNoContent {
			t.Errorf("%q answered %d, want 204", body, rr.Code)
		}
	}
}

// The policy has to name the endpoint both ways, or the browsers that only
// implement one of them report nowhere.
func TestPolicyNamesTheReportEndpoint(t *testing.T) {
	e := seeded(t)
	h := e.do(http.MethodGet, "/b/demo", nil).Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"report-uri " + cspReportPath, "report-to csp"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy does not carry %q: %s", want, csp)
		}
	}
	if got := h.Get("Reporting-Endpoints"); !strings.Contains(got, cspReportPath) {
		t.Errorf("Reporting-Endpoints = %q, and report-to names nowhere without it", got)
	}
}
