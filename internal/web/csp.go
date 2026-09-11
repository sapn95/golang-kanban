package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// cspReportPath is where a browser posts a Content-Security-Policy violation.
// Named twice on the way out, because the two ways of asking are at different
// points in their lives: report-uri, in the policy itself, is deprecated and is
// the only one Firefox and Safari implement; report-to names an endpoint group
// that a Reporting-Endpoints header then points at this path. Both arrive here.
const cspReportPath = "/csp-report"

// maxReportsPerRequest is how many violations one request may write to the log.
// A browser batches a few; the number exists because the endpoint takes a
// megabyte of JSON from anybody who can reach the port.
const maxReportsPerRequest = 20

// cspReport is one violation, in either of the two shapes a browser sends.
//
// report-uri posts `{"csp-report": {...}}` as application/csp-report, with
// hyphenated keys. report-to posts an array of reports as
// application/reports+json, with camelCase keys and the payload under "body".
// Nothing is shared between them but the meaning, so both are read here and
// flattened into one line.
type cspReport struct {
	Document  string `json:"document-uri"`
	Directive string `json:"violated-directive"`
	Effective string `json:"effective-directive"`
	Blocked   string `json:"blocked-uri"`
	Source    string `json:"source-file"`
	Line      int    `json:"line-number"`
	Sample    string `json:"script-sample"`
}

type cspReportEnvelope struct {
	Report cspReport `json:"csp-report"`
}

type reportToEntry struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	Body struct {
		Document  string `json:"documentURL"`
		Effective string `json:"effectiveDirective"`
		Blocked   string `json:"blockedURL"`
		Source    string `json:"sourceFile"`
		Line      int    `json:"lineNumber"`
		Sample    string `json:"sample"`
	} `json:"body"`
}

// cspViolation records what a browser refused to load.
//
// A report is the only way to learn what a policy costs somebody else: a page
// that works here can be missing a script on another machine, in another
// browser, or behind something that injects one, and the person looking at it
// sees a feature that does not work rather than a policy that refused it. The
// log line is the answer to "what is being blocked" without asking them to open
// a console.
//
// Answers 204 whatever arrives. A browser is not waiting for anything and has
// nowhere to put an error, and a report that cannot be read is still a fact
// worth one line.
func (s *Server) cspViolation(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var reports []cspReport
	switch {
	case strings.HasPrefix(strings.TrimSpace(string(body)), "["):
		var entries []reportToEntry
		if err := json.Unmarshal(body, &entries); err == nil {
			for _, e := range entries {
				if e.Type != "" && e.Type != "csp-violation" {
					continue
				}
				// A directive is the one field a violation cannot be without,
				// so an entry with none is not one. Without this, an array of
				// empty objects wrote a line of blanks for each.
				if e.Body.Effective == "" {
					continue
				}
				if len(reports) >= maxReportsPerRequest {
					s.log.Warn("csp report with more entries than a browser sends",
						"entries", len(entries), "logging", maxReportsPerRequest)
					break
				}
				reports = append(reports, cspReport{
					Document: e.Body.Document, Effective: e.Body.Effective,
					Blocked: e.Body.Blocked, Source: e.Body.Source,
					Line: e.Body.Line, Sample: e.Body.Sample,
				})
			}
		}
	default:
		var env cspReportEnvelope
		// A directive is the one field a violation cannot be without, so an
		// empty envelope logs as unparsed rather than as a line of blanks.
		if err := json.Unmarshal(body, &env); err == nil &&
			(env.Report.Effective != "" || env.Report.Directive != "") {
			reports = append(reports, env.Report)
		}
	}

	if len(reports) == 0 {
		s.log.Warn("csp report that did not parse", "bytes", len(body))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, c := range reports {
		directive := c.Effective
		if directive == "" {
			directive = c.Directive
		}
		attrs := []any{"directive", directive, "blocked", c.Blocked, "document", c.Document}
		if c.Source != "" {
			attrs = append(attrs, "source", c.Source, "line", c.Line)
		}
		if c.Sample != "" {
			attrs = append(attrs, "sample", c.Sample)
		}
		// Warn rather than Error. A browser extension that injects a script
		// produces one of these on a page that is perfectly fine, so this is
		// something to read rather than something to wake up for.
		s.log.LogAttrs(r.Context(), slog.LevelWarn, "content security policy refused something",
			attrsOf(attrs)...)
	}
	w.WriteHeader(http.StatusNoContent)
}

// attrsOf turns the key/value pairs above into slog attributes, so the log line
// is built once rather than in three branches.
func attrsOf(kv []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, slog.Any(kv[i].(string), kv[i+1]))
	}
	return out
}
