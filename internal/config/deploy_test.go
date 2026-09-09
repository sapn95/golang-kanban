package config

import (
	"encoding/xml"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The deployment templates under deploy/ name environment variables that only
// this package reads. A name spelled wrong there is a setting that does nothing
// at all: the process starts, takes the default, and the operator is left with a
// file that says otherwise. Nothing else in the build compares the two lists.
func TestDeploymentTemplatesNameVariablesThisPackageReads(t *testing.T) {
	cfg, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
	known := map[string]bool{}
	for _, s := range cfg.Settings() {
		known[s.Name] = true
	}
	// The one variable FromEnv reads without a field of its own: a fallback for
	// BACKUP_S3_REGION, so credentials that are in the environment for something
	// else bring their region with them.
	known["AWS_REGION"] = true

	for _, tc := range []struct {
		file  string
		names func(*testing.T, []byte) []string
	}{
		{"../../docker-compose.yml", composeEnvNames},
		{"../../deploy/compose/compose.yaml", composeEnvNames},
		{"../../deploy/unraid/kanban.xml", unraidEnvNames},
		{"../../deploy/helm/kanban/templates/deployment.yaml", manifestEnvNames},
	} {
		body, err := os.ReadFile(tc.file)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		names := tc.names(t, body)
		if len(names) == 0 {
			t.Errorf("%s: no variable found in it, so this test is reading the wrong thing", tc.file)
		}
		for _, name := range names {
			if known[name] || foreignVariable(name) {
				continue
			}
			t.Errorf("%s sets %s, which no field of Config reads", tc.file, name)
		}
	}
}

// foreignVariable reports whether a name configures one of the other images in
// those files. The database container and the oauth2-proxy sidecar read their
// own variables, and this package never sees them.
func foreignVariable(name string) bool {
	return strings.HasPrefix(name, "POSTGRES_") ||
		strings.HasPrefix(name, "OAUTH2_PROXY_") ||
		name == "PGDATA"
}

// composeEnvNames returns the keys of the environment mappings in a compose
// file. Both compose files write them as a mapping with the name on the left,
// which a pattern can find without a YAML parser in go.mod for one test.
var composeEnvKey = regexp.MustCompile(`(?m)^\s{4,}([A-Z][A-Z0-9_]{2,}):`)

func composeEnvNames(_ *testing.T, body []byte) []string {
	return submatches(composeEnvKey, body)
}

// manifestEnvNames returns the env names of a Kubernetes manifest. Resource
// names are lowercase with dashes, so the shape of the name is enough to tell
// the two uses of `name:` apart.
var manifestEnvName = regexp.MustCompile(`(?m)^\s*- name: ([A-Z][A-Z0-9_]{2,})\s*$`)

func manifestEnvNames(_ *testing.T, body []byte) []string {
	return submatches(manifestEnvName, body)
}

// unraidEnvNames returns the variables of an Unraid container template. Its
// Config elements carry ports and paths as well, and Type tells them apart.
func unraidEnvNames(t *testing.T, body []byte) []string {
	var tmpl struct {
		Config []struct {
			Target string `xml:"Target,attr"`
			Type   string `xml:"Type,attr"`
		} `xml:"Config"`
	}
	if err := xml.Unmarshal(body, &tmpl); err != nil {
		t.Fatalf("the Unraid template does not parse, so Unraid would not read it either: %v", err)
	}
	var out []string
	for _, c := range tmpl.Config {
		if c.Type == "Variable" {
			out = append(out, c.Target)
		}
	}
	return out
}

func submatches(re *regexp.Regexp, body []byte) []string {
	var out []string
	for _, m := range re.FindAllSubmatch(body, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// .env.example is the file an operator copies and edits, so the two directions
// of drift both matter: a name in it that compose never reads is a knob that
// turns nothing, and a name compose reads that is missing from it is a knob
// nobody finds.
func TestComposeEnvExampleAndComposeFileAgree(t *testing.T) {
	example, err := os.ReadFile("../../deploy/compose/.env.example")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile("../../deploy/compose/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, name := range submatches(regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)=`), example) {
		documented[name] = true
	}
	interpolated := map[string]bool{}
	for _, name := range submatches(regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)`), compose) {
		interpolated[name] = true
	}
	if len(documented) == 0 || len(interpolated) == 0 {
		t.Fatalf("%d documented, %d interpolated; one of the two files is not being read", len(documented), len(interpolated))
	}
	for name := range documented {
		if !interpolated[name] {
			t.Errorf(".env.example sets %s and compose.yaml does not use it", name)
		}
	}
	for name := range interpolated {
		if !documented[name] {
			t.Errorf("compose.yaml reads %s from the environment and .env.example does not mention it", name)
		}
	}
}
