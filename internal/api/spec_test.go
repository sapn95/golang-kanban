package api

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"kanban/internal/model"
	"kanban/internal/service"
)

// openapi.json is hand-written, and a hand-written contract nobody checks
// describes last month's API. Three tests hold it to the code: the paths
// against the registrations in api.go, every schema's properties against the
// json tags of the type that fills it, and the length caps against the service
// constants they claim to mirror. api_test.go adds the fourth, which validates
// every response the suite provokes against the schema declared for it.

func TestEveryRouteIsInTheSpec(t *testing.T) {
	registered := routesInSource(t, "api.go")
	documented := routesInSpec(t)

	for _, r := range registered {
		if !slices.Contains(documented, r) {
			t.Errorf("%s is registered in api.go and not in openapi.json", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(registered, d) {
			t.Errorf("openapi.json describes %s, which is not registered in api.go", d)
		}
	}
}

var (
	// mux.HandleFunc("GET /api/v1/boards/{board}", ...). Anchored at the start
	// of a statement, so a registration that is commented out is not a route.
	sourceRoute = regexp.MustCompile(`(?m)^\t*mux\.Handle(?:Func)?\("([A-Z]+ /[^"]*)"`)
	// The same call without its pattern, which is how a registration the
	// pattern above cannot read is still counted.
	registerCall = regexp.MustCompile(`(?m)^\t*mux\.Handle(?:Func)?\(`)
)

// routesInSource is the side that can fail quietly: a registration the regexp
// does not match is a route this test then never looks for, and if the spec is
// missing it too, both comparisons pass. So the calls are counted as well as
// the patterns.
func routesInSource(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, m := range sourceRoute.FindAllStringSubmatch(string(src), -1) {
		method, pattern, _ := strings.Cut(m[1], " ")
		// {$} only says "and nothing after the slash", which a path in a spec
		// says by being the path.
		pattern = strings.TrimSuffix(pattern, "{$}")
		found = append(found, method+" "+pattern)
	}
	if calls := len(registerCall.FindAllString(string(src), -1)); len(found) != calls {
		t.Fatalf("%s registers %d routes and the pattern found %d of them: %v",
			path, calls, len(found), found)
	}
	slices.Sort(found)
	return found
}

func routesInSpec(t *testing.T) []string {
	t.Helper()
	var found []string
	for path, item := range spec(t).paths() {
		for method := range item {
			if m := strings.ToUpper(method); isMethod(m) {
				found = append(found, m+" "+path)
			}
		}
	}
	slices.Sort(found)
	return found
}

func isMethod(s string) bool {
	return slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}, s)
}

// schemaOf names the schema each wire type fills. Every type in dto.go is in
// here and every schema in the document is claimed by one of them, so a new DTO
// without a schema fails, and a schema for a type that no longer exists fails
// too.
var schemaOf = map[string]any{
	"Index":         indexBody{},
	"Error":         errorBody{},
	"Board":         boardBody{},
	"Column":        columnBody{},
	"SLA":           slaBody{},
	"Label":         labelBody{},
	"Card":          cardBody{},
	"Subtask":       subtaskBody{},
	"Comment":       commentBody{},
	"BoardInput":    boardInput{},
	"BoardPatch":    boardPatch{},
	"LayoutInput":   layoutInput{},
	"SLAInput":      slaInput{},
	"ColumnInput":   columnInput{},
	"ColumnPatch":   columnPatch{},
	"OrderInput":    orderInput{},
	"LabelInput":    labelInput{},
	"LabelPatch":    labelPatch{},
	"CardInput":     cardInput{},
	"SubtaskInput":  subtaskInput{},
	"AssigneeInput": assigneeInput{},
	"CommentInput":  commentInput{},
	"BulkInput":     bulkInput{},
	"BulkResult":    bulkBody{},
}

// responseSchemas are the ones a handler writes. For those the required list is
// not a judgement call: a field without omitempty is always in the JSON, so it
// is required, and one with omitempty never is.
var responseSchemas = []string{
	"Index", "Error", "Board", "Column", "SLA", "Label", "Card", "Subtask", "Comment", "BulkResult",
}

func TestSchemaPropertiesMatchTheGoTypes(t *testing.T) {
	sp := spec(t)
	schemas := sp.schemas()

	for name := range schemas {
		if _, ok := schemaOf[name]; !ok {
			t.Errorf("openapi.json has a schema %s that no type in dto.go fills", name)
		}
	}

	for name, v := range schemaOf {
		schema, ok := schemas[name].(map[string]any)
		if !ok {
			t.Errorf("openapi.json has no schema %s", name)
			continue
		}
		props, _ := schema["properties"].(map[string]any)
		fields, optional := jsonFields(reflect.TypeOf(v))

		for _, f := range fields {
			if _, ok := props[f]; !ok {
				t.Errorf("%s: %T has a field %q and the schema does not", name, v, f)
			}
		}
		for p := range props {
			if !slices.Contains(fields, p) {
				t.Errorf("%s: the schema has a property %q and %T does not", name, p, v)
			}
		}

		required := strList(schema["required"])
		for _, r := range required {
			if _, ok := props[r]; !ok {
				t.Errorf("%s: %q is required and is not a property", name, r)
			}
		}
		if !slices.Contains(responseSchemas, name) {
			continue
		}
		for _, f := range fields {
			want := !optional[f]
			if got := slices.Contains(required, f); got != want {
				t.Errorf("%s: %q is %s in the schema and %s in %T",
					name, f, requiredWord(got), requiredWord(want), v)
			}
		}
	}
}

func requiredWord(required bool) string {
	if required {
		return "required"
	}
	return "optional"
}

// jsonFields returns the wire names of a struct's fields, and which of them can
// be absent from the JSON.
func jsonFields(t reflect.Type) (names []string, optional map[string]bool) {
	optional = map[string]bool{}
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		names = append(names, name)
		optional[name] = strings.Contains(opts, "omitempty")
	}
	sort.Strings(names)
	return names, optional
}

// TestTheSpecStatesTheServiceLimits keeps the numbers in the document the
// numbers the service enforces. Without it the spec is where a caller reads
// that a title may be 200 characters long after the constant went to 300.
func TestTheSpecStatesTheServiceLimits(t *testing.T) {
	limits := []struct {
		schema, property, keyword string
		want                      int
	}{
		{"BoardInput", "name", "maxLength", service.MaxName},
		{"BoardInput", "slug", "maxLength", service.MaxSlug},
		{"BoardPatch", "name", "maxLength", service.MaxName},
		{"BoardPatch", "slug", "maxLength", service.MaxSlug},
		{"ColumnInput", "name", "maxLength", service.MaxName},
		{"LabelInput", "name", "maxLength", service.MaxName},
		{"CardInput", "title", "maxLength", service.MaxTitle},
		{"CardInput", "description", "maxLength", service.MaxDescription},
		{"CardInput", "assignee", "maxLength", service.MaxAssignee},
		{"CardInput", "subtasks", "maxItems", service.MaxSubtasks},
		{"SubtaskInput", "title", "maxLength", service.MaxTitle},
		{"AssigneeInput", "assignee", "maxLength", service.MaxAssignee},
		{"CommentInput", "body", "maxLength", service.MaxComment},
		{"BulkInput", "ids", "maxItems", service.MaxBulk},
		{"SLAInput", "response_hours", "maximum", model.MaxResponseHours},
	}
	schemas := spec(t).schemas()
	for _, l := range limits {
		schema, _ := schemas[l.schema].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		prop, ok := props[l.property].(map[string]any)
		if !ok {
			t.Errorf("%s.%s is not in openapi.json", l.schema, l.property)
			continue
		}
		got, ok := prop[l.keyword].(float64)
		if !ok {
			t.Errorf("%s.%s has no %s", l.schema, l.property, l.keyword)
			continue
		}
		if int(got) != l.want {
			t.Errorf("%s.%s %s is %d and the service allows %d",
				l.schema, l.property, l.keyword, int(got), l.want)
		}
	}

	// The nested schema carries the limit for one entry; the array carries the
	// limit for the list. A checklist of 100 entries of 200 characters is the
	// biggest card the service accepts, and both halves have to be written down
	// for that to be readable off the document.
	bulk := spec(t).ref("#/components/schemas/BulkInput")
	if bulk == nil {
		t.Fatal("BulkInput does not resolve")
	}
	if actions := len(enumOf(t, bulk, "action")); actions != 4 {
		t.Errorf("BulkInput.action lists %d actions and the service has 4", actions)
	}
}

func enumOf(t *testing.T, schema map[string]any, property string) []string {
	t.Helper()
	props, _ := schema["properties"].(map[string]any)
	prop, _ := props[property].(map[string]any)
	return strList(prop["enum"])
}

// TestTheSearchExamplesUseFiltersTheServiceHonours reads the examples on the q
// parameter and puts them through the parser. An unrecognised prefix is not
// refused, it is searched for as text, so is:overdue in an example looks like a
// filter and finds nothing. That is the one mistake in this document a reader
// cannot spot and no other test would catch.
func TestTheSearchExamplesUseFiltersTheServiceHonours(t *testing.T) {
	examples := searchExamples(t)
	if len(examples) == 0 {
		t.Fatal("the q parameter has no examples, and this test then checks nothing")
	}
	for name, value := range examples {
		q := service.ParseQuery(value)
		filters := 0
		for _, tok := range strings.Fields(value) {
			key, _, ok := strings.Cut(tok, ":")
			if !ok {
				continue
			}
			if slices.Contains(q.Text, strings.ToLower(tok)) {
				t.Errorf("example %q searches for %q as text: %s: is not a filter the service has",
					name, tok, key)
				continue
			}
			filters++
		}
		if filters > 0 && q.Empty() {
			t.Errorf("example %q reads like a filter and matches every card", name)
		}
	}
}

// searchExamples collects the examples of every q parameter in the document, by
// name. Walking is what makes it hold for the next path that takes a search
// rather than only for the one that does today.
func searchExamples(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if v["name"] == "q" && v["in"] == "query" {
				examples, _ := v["examples"].(map[string]any)
				for name, e := range examples {
					entry, _ := e.(map[string]any)
					if value, ok := entry["value"].(string); ok {
						out[name] = value
					}
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(spec(t).doc)
	return out
}

func TestEveryRefResolves(t *testing.T) {
	sp := spec(t)
	var walk func(v any, where string)
	walk = func(v any, where string) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				if sp.ref(ref) == nil {
					t.Errorf("%s: %s does not resolve", where, ref)
				}
			}
			for k, child := range v {
				walk(child, where+"/"+k)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s/%d", where, i))
			}
		}
	}
	walk(sp.doc, "")
}

// --- reading the document -----------------------------------------------------

type openAPIDoc struct {
	doc map[string]any
}

// spec parses the embedded document, so the test reads the same bytes the
// handler serves rather than the file next to it.
func spec(t *testing.T) *openAPIDoc {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(specJSON, &doc); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	return &openAPIDoc{doc: doc}
}

func (d *openAPIDoc) paths() map[string]map[string]any {
	out := map[string]map[string]any{}
	paths, _ := d.doc["paths"].(map[string]any)
	for path, item := range paths {
		if m, ok := item.(map[string]any); ok {
			out[path] = m
		}
	}
	return out
}

func (d *openAPIDoc) schemas() map[string]any {
	components, _ := d.doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	return schemas
}

// ref resolves a local reference. Remote ones are not used and would be a way
// for the contract to live somewhere the tests cannot see it.
func (d *openAPIDoc) ref(ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = d.doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[part]; !ok {
			return nil
		}
	}
	out, _ := cur.(map[string]any)
	return out
}

// resolve follows a $ref once, which is as deep as this document nests them.
func (d *openAPIDoc) resolve(schema map[string]any) map[string]any {
	if ref, ok := schema["$ref"].(string); ok {
		if target := d.ref(ref); target != nil {
			return target
		}
	}
	return schema
}

// operation finds the operation for a request path, matching a {name} segment
// against any one segment the way the mux does.
//
// More than one template can match: /boards/{board}/columns/order is also
// /boards/{board}/columns/{column} with "order" for the id. The mux gives the
// more specific pattern precedence, so the template with the most literal
// segments wins here too, and the query string is not part of either.
func (d *openAPIDoc) operation(method, path string) (map[string]any, string) {
	path, _, _ = strings.Cut(path, "?")
	want := strings.Split(strings.TrimSuffix(path, "/"), "/")

	best, literals := "", -1
	for template := range d.paths() {
		got := strings.Split(strings.TrimSuffix(template, "/"), "/")
		if len(got) != len(want) {
			continue
		}
		match, exact := true, 0
		for i := range got {
			switch {
			case strings.HasPrefix(got[i], "{"):
			case got[i] == want[i]:
				exact++
			default:
				match = false
			}
			if !match {
				break
			}
		}
		if match && exact > literals {
			best, literals = template, exact
		}
	}
	if best == "" {
		return nil, ""
	}
	op, _ := d.paths()[best][strings.ToLower(method)].(map[string]any)
	return op, best
}

// responseSchema is the schema the document declares for one answer, and
// whether it declares a body at all. An exact status wins over the default,
// which is what carries the error body for every endpoint.
func (d *openAPIDoc) responseSchema(method, path string, status int) (schema map[string]any, hasBody, found bool) {
	op, _ := d.operation(method, path)
	if op == nil {
		return nil, false, false
	}
	responses, _ := op["responses"].(map[string]any)
	entry, ok := responses[fmt.Sprint(status)].(map[string]any)
	if !ok {
		if entry, ok = responses["default"].(map[string]any); !ok {
			return nil, false, false
		}
	}
	entry = d.resolve(entry)
	content, ok := entry["content"].(map[string]any)
	if !ok {
		return nil, false, true
	}
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		return nil, false, true
	}
	s, ok := media["schema"].(map[string]any)
	if !ok {
		return nil, false, true
	}
	return s, true, true
}

// --- the validator ------------------------------------------------------------

// validate checks a decoded JSON value against the subset of JSON Schema this
// document uses. A validator is a dependency the project is not taking on for
// one file, and the subset is small because the schemas are hand-written and
// stay in it: the keywords below are all of them, and an unknown keyword in the
// document is reported rather than ignored, so the day a schema grows a oneOf
// this test says so instead of passing everything.
func (d *openAPIDoc) validate(schema map[string]any, v any, where string) []string {
	schema = d.resolve(schema)
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, where+": "+fmt.Sprintf(format, args...))
	}

	for k := range schema {
		switch k {
		case "$ref", "type", "enum", "properties", "required", "additionalProperties",
			"items", "maxLength", "minLength", "maxItems", "minItems", "minimum",
			"pattern", "format", "description", "summary", "example", "examples",
			"title", "default", "deprecated":
		default:
			add("the schema uses %q, which this validator does not check", k)
		}
	}

	types := strList(schema["type"])
	if len(types) > 0 && !hasType(types, v) {
		return append(problems, fmt.Sprintf("%s: is %s and the schema says %s",
			where, jsonType(v), strings.Join(types, " or ")))
	}

	if enum := strList(schema["enum"]); len(enum) > 0 {
		if s, ok := v.(string); !ok || !slices.Contains(enum, s) {
			add("%v is not one of %v", v, enum)
		}
	}

	switch val := v.(type) {
	case string:
		if max, ok := schema["maxLength"].(float64); ok && len([]rune(val)) > int(max) {
			add("is %d characters, the schema allows %d", len([]rune(val)), int(max))
		}
		if min, ok := schema["minLength"].(float64); ok && len([]rune(val)) < int(min) {
			add("is %d characters, the schema wants at least %d", len([]rune(val)), int(min))
		}
		if pattern, ok := schema["pattern"].(string); ok {
			re, err := regexp.Compile(pattern)
			switch {
			case err != nil:
				add("the schema pattern %q does not compile: %v", pattern, err)
			case !re.MatchString(val):
				add("%q does not match %q", val, pattern)
			}
		}
		if schema["format"] == "date-time" && val != "" {
			if _, err := time.Parse(time.RFC3339, val); err != nil {
				add("%q is not an RFC 3339 timestamp", val)
			}
		}
	case float64:
		if min, ok := schema["minimum"].(float64); ok && val < min {
			add("is %v, the schema wants at least %v", val, min)
		}
		if slices.Contains(types, "integer") && val != float64(int64(val)) {
			add("is %v, the schema says integer", val)
		}
	case []any:
		if max, ok := schema["maxItems"].(float64); ok && len(val) > int(max) {
			add("has %d items, the schema allows %d", len(val), int(max))
		}
		if min, ok := schema["minItems"].(float64); ok && len(val) < int(min) {
			add("has %d items, the schema wants at least %d", len(val), int(min))
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range val {
				problems = append(problems, d.validate(items, item, fmt.Sprintf("%s[%d]", where, i))...)
			}
		}
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for _, r := range strList(schema["required"]) {
			if _, ok := val[r]; !ok {
				add("%q is required and absent", r)
			}
		}
		for name, child := range val {
			if prop, ok := props[name].(map[string]any); ok {
				problems = append(problems, d.validate(prop, child, where+"."+name)...)
				continue
			}
			switch extra := schema["additionalProperties"].(type) {
			case bool:
				if !extra {
					add("has %q, which the schema does not allow", name)
				}
			case map[string]any:
				problems = append(problems, d.validate(extra, child, where+"."+name)...)
			}
		}
	}
	return problems
}

func hasType(types []string, v any) bool {
	return slices.Contains(types, jsonType(v)) ||
		// An integer is a number whose value happens to be whole; the check
		// that it is whole is above.
		(jsonType(v) == "number" && slices.Contains(types, "integer"))
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func strList(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
