// Package uci implements a minimal read-only parser for OpenWrt UCI
// configuration files (the /etc/config/* format). The daemon only reads the
// configuration; modifications happen through LuCI or the uci CLI.
package uci

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Section is one `config` block of a UCI file. Anonymous sections get a
// generated name of the form anon<N>.
type Section struct {
	Type    string
	Name    string
	Options map[string]string
	Lists   map[string][]string
}

// Get returns the value of an option, or def when it is not set.
func (s *Section) Get(key, def string) string {
	if v, ok := s.Options[key]; ok {
		return v
	}
	return def
}

// GetBool interprets an option as a boolean (UCI truthy values are "1",
// "true", "yes", "on").
func (s *Section) GetBool(key string, def bool) bool {
	v, ok := s.Options[key]
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// GetInt interprets an option as an integer, returning def on absence or a
// parse error.
func (s *Section) GetInt(key string, def int) int {
	v, ok := s.Options[key]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// List returns all values of a list option; it never returns nil.
func (s *Section) List(key string) []string {
	if v, ok := s.Lists[key]; ok {
		return v
	}
	return []string{}
}

// Config is a parsed UCI file.
type Config struct {
	Sections []*Section
}

// SectionsOfType returns all sections with the given type, in file order.
func (c *Config) SectionsOfType(typ string) []*Section {
	var out []*Section
	for _, s := range c.Sections {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

// Named returns the section with the given type and name.
func (c *Config) Named(typ, name string) *Section {
	for _, s := range c.Sections {
		if s.Type == typ && s.Name == name {
			return s
		}
	}
	return nil
}

// Parse parses the UCI config format. Unknown directives and malformed lines
// are collected into the returned config error list but do not stop parsing.
func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	var errs []error
	var cur *Section
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	anon := 0
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields, err := splitFields(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", lineNo, err))
			continue
		}
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "config":
			if len(fields) < 2 {
				errs = append(errs, fmt.Errorf("line %d: config without type", lineNo))
				cur = nil
				continue
			}
			name := ""
			if len(fields) >= 3 {
				name = fields[2]
			} else {
				anon++
				name = fmt.Sprintf("anon%d", anon)
			}
			cur = &Section{Type: fields[1], Name: name, Options: map[string]string{}, Lists: map[string][]string{}}
			cfg.Sections = append(cfg.Sections, cur)
		case "option":
			if cur == nil || len(fields) < 3 {
				errs = append(errs, fmt.Errorf("line %d: option outside section or without value", lineNo))
				continue
			}
			// Unquoted values may contain spaces only inside quotes, which
			// splitFields already merged; join defensively.
			cur.Options[fields[1]] = strings.Join(fields[2:], " ")
		case "list":
			if cur == nil || len(fields) < 3 {
				errs = append(errs, fmt.Errorf("line %d: list outside section or without value", lineNo))
				continue
			}
			cur.Lists[fields[1]] = append(cur.Lists[fields[1]], strings.Join(fields[2:], " "))
		default:
			errs = append(errs, fmt.Errorf("line %d: unknown directive %q", lineNo, fields[0]))
		}
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, err)
	}
	return cfg, errors.Join(errs...)
}

// ParseFile reads and parses a UCI file.
func ParseFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// splitFields splits a UCI line into tokens. Tokens may be single- or
// double-quoted, in which case they can contain spaces; the quotes are
// stripped. Escaped quotes inside quoted tokens are supported.
func splitFields(line string) ([]string, error) {
	var fields []string
	var b strings.Builder
	inQuote := byte(0)
	flush := func() {
		if b.Len() > 0 {
			fields = append(fields, b.String())
			b.Reset()
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote != 0:
			if c == '\\' && i+1 < len(line) && line[i+1] == inQuote {
				b.WriteByte(inQuote)
				i++
				continue
			}
			if c == inQuote {
				inQuote = 0
				fields = append(fields, b.String())
				b.Reset()
				continue
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			// A quote directly after another token starts a new token only
			// when preceded by whitespace; handled because flush already ran.
			inQuote = c
			if b.Len() > 0 {
				// e.g. option foo bar'baz — treat accumulated bytes as a
				// token and continue quoted part separately.
				flush()
			}
		case c == ' ' || c == '\t':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	if inQuote != 0 {
		return nil, errors.New("unterminated quote")
	}
	flush()
	return fields, nil
}
