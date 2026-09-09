package config

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"time"
)

// maskSet stands in for a secret that is set. Whether a credential is there at
// all is the question a broken deployment raises; what it says is not, and a
// doctor report gets pasted into issues and chats.
const maskSet = "[set]"

// Redacted returns the configuration with the secrets taken out, so that the
// whole of it can be printed. It reads the `secret` tags rather than naming
// fields, so a new credential is covered by tagging it and nothing else.
func (c Config) Redacted() Config {
	rv := reflect.ValueOf(&c).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rv.Field(i)
		if f.Kind() != reflect.String || f.String() == "" {
			continue
		}
		switch rt.Field(i).Tag.Get("secret") {
		case "true":
			f.SetString(maskSet)
		case "url":
			f.SetString(redactURL(f.String()))
		}
	}
	return c
}

// RedactedDSN is the connection string with the password taken out: the one
// line that answers which database the process actually tried, without putting
// the credential on someone's terminal.
func (c Config) RedactedDSN() string { return redactURL(c.DSN()) }

// redactURL keeps an address readable and takes the password out of it.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		// A connection string this package could not parse may still be one
		// with a password in it, so none of it is printed.
		return maskSet
	}
	return u.Redacted()
}

// Setting is one environment variable and the value this process ended up with,
// after the defaults and with the secrets masked.
type Setting struct {
	Name   string
	Value  string
	Secret bool
}

// Settings lists every variable Config reads, in the order the fields are
// declared, which groups them the way the readme does. `kanban doctor` prints
// it, and the empty value of a variable that is not set is part of the answer:
// the usual reason a deployment misbehaves is a name that was spelled wrong
// somewhere else and never arrived here.
func (c Config) Settings() []Setting {
	red := reflect.ValueOf(c.Redacted())
	rt := red.Type()
	out := make([]Setting, 0, rt.NumField())
	for i := range rt.NumField() {
		field := rt.Field(i)
		name, ok := field.Tag.Lookup("env")
		if !ok {
			continue
		}
		out = append(out, Setting{
			Name:   name,
			Value:  settingValue(red.Field(i)),
			Secret: field.Tag.Get("secret") != "",
		})
	}
	return out
}

// settingValue renders one field for a report.
func settingValue(v reflect.Value) string {
	switch value := v.Interface().(type) {
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case int:
		return strconv.Itoa(value)
	case time.Duration:
		return value.String()
	case map[string]string:
		if len(value) == 0 {
			return ""
		}
		// The addresses are personal data, and the count is what the operator
		// is checking: whether the pairs they set arrived.
		if len(value) == 1 {
			return "1 pair"
		}
		return fmt.Sprintf("%d pairs", len(value))
	default:
		return fmt.Sprint(value)
	}
}
