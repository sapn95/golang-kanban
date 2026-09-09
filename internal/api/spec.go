package api

import (
	_ "embed"
	"net/http"
)

// The document is hand-written and lives next to the handlers it describes.
// Nothing generates it, because a generator for Go source is either a
// dependency with an opinion about how handlers are written or a set of magic
// comments, and both cost more than one file that two tests hold to the code:
// one compares the paths against the registrations in api.go in both
// directions, the other compares every schema's properties against the json
// tags of the type that fills it.
//
// It is JSON rather than YAML for the same reason: encoding/json can read it in
// a test, and a YAML parser would be a dependency the project would carry for
// exactly one file.
//
//go:embed openapi.json
var specJSON []byte

// openAPI serves the document. There is no swagger-ui in here and there is not
// going to be one: it is about a megabyte of vendored JavaScript in a project
// whose selling point is that it has none, and anybody who wants a browsable
// version can point their own at this URL.
func (s *Server) openAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(specJSON)
}
