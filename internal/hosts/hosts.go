// Package hosts discovers concrete SSH aliases. The system SSH client remains
// responsible for interpreting connection settings.
package hosts

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// ValidAlias reports whether a name can be passed to ssh as a single host.
func ValidAlias(s string) bool {
	return validSelector(s) && !strings.ContainsAny(s, "*?")
}

func validSelector(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) && !strings.ContainsRune("._:-%*?", r) {
			return false
		}
	}
	return true
}

// Select discovers aliases in declaration order, then adds literal requested
// targets. Patterns filter the resulting list and exclusions always win.
func Select(config string, include, exclude []string) ([]string, error) {
	for _, group := range [][]string{include, exclude} {
		for _, selector := range group {
			if !validSelector(selector) {
				return nil, fmt.Errorf("invalid SSH host selector %q (wildcards: * and ?)", selector)
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	d := discovery{home: home, seen: make(map[string]bool)}
	useDefault := config == ""
	if useDefault {
		config = filepath.Join(home, ".ssh", "config")
	}
	if err := d.read(config, nil, 0); err != nil {
		var missing *os.PathError
		if !useDefault || !errors.As(err, &missing) || missing.Path != config || !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	for _, name := range include {
		if ValidAlias(name) {
			d.add(name)
		}
	}
	selected := make([]string, 0, len(d.names))
	for _, name := range d.names {
		if (len(include) == 0 || matches(include, name)) && !matches(exclude, name) {
			selected = append(selected, name)
		}
	}
	return selected, nil
}

type discovery struct {
	home  string
	seen  map[string]bool
	names []string
}

func (d *discovery) add(name string) {
	if !d.seen[name] {
		d.seen[name] = true
		d.names = append(d.names, name)
	}
}

// matches implements the Host directive's positive and negated patterns.
// SSH Host wildcards are '*' and '?' only.
func matches(patterns []string, name string) bool {
	positive := false
	for _, p := range patterns {
		pattern, neg := strings.CutPrefix(p, "!")
		pattern = strings.ReplaceAll(strings.ReplaceAll(pattern, `\`, `\\`), "[", `\[`)
		hit, _ := path.Match(pattern, name)
		if hit && neg {
			return false
		}
		positive = positive || hit
	}
	return positive
}

func inScope(name string, enclosing [][]string) bool {
	for _, patterns := range enclosing {
		if !matches(patterns, name) {
			return false
		}
	}
	return true
}

func (d *discovery) read(filename string, enclosing [][]string, depth int) error {
	if depth > 16 {
		return fmt.Errorf("%s: too many nested SSH includes (possible cycle)", filename)
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 4096), 1<<20)
	var block []string
	for line := 1; scan.Scan(); line++ {
		s := strings.TrimSpace(strings.TrimPrefix(scan.Text(), "\uFEFF"))
		at := strings.IndexAny(s, " \t=")
		if at < 0 {
			at = len(s)
		}
		key := strings.ToLower(s[:at])
		if key != "host" && key != "include" && key != "match" {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s[at:]), "="))
		args, err := words(value)
		if err != nil || len(args) == 0 {
			return fmt.Errorf("%s:%d: invalid %s directive", filename, line, key)
		}
		switch key {
		case "host":
			block = args
			for _, alias := range args {
				if ValidAlias(alias) && matches(block, alias) && inScope(alias, enclosing) {
					d.add(alias)
				}
			}
		case "match":
			// Match exec and other runtime predicates cannot identify hosts
			// statically. Only Match all unconditionally admits includes.
			block = []string{}
			if len(args) == 1 && strings.EqualFold(args[0], "all") {
				block = nil
			}
		case "include":
			if block != nil && len(block) == 0 {
				continue
			}
			scope := enclosing
			if len(block) > 0 {
				scope = append(scope, block)
			}
			for _, arg := range args {
				files, err := d.includeFiles(arg)
				if err != nil {
					return fmt.Errorf("%s:%d: %w", filename, line, err)
				}
				for _, file := range files {
					if err := d.read(file, scope, depth+1); err != nil {
						return err
					}
				}
			}
		}
	}
	return scan.Err()
}

func (d *discovery) includeFiles(arg string) ([]string, error) {
	// Host-dependent tokens and named-user homes are resolved by ssh.
	var value strings.Builder
	for i := 0; i < len(arg); i++ {
		switch {
		case strings.HasPrefix(arg[i:], "${"):
			key, _, ok := strings.Cut(arg[i+2:], "}")
			v, exists := os.LookupEnv(key)
			if !ok || !exists || v == "" {
				return nil, nil
			}
			value.WriteString(v)
			i += len(key) + 2
		case strings.HasPrefix(arg[i:], "%d"):
			value.WriteString(d.home)
			i++
		case strings.HasPrefix(arg[i:], "%%"):
			value.WriteByte('%')
			i++
		case arg[i] == '%':
			return nil, nil
		default:
			value.WriteByte(arg[i])
		}
	}
	name := filepath.FromSlash(value.String())
	if strings.HasPrefix(name, "~"+string(filepath.Separator)) {
		name = filepath.Join(d.home, name[2:])
	} else if strings.HasPrefix(name, "~") {
		return nil, nil
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(d.home, ".ssh", name)
	}
	files, err := filepath.Glob(name)
	if err != nil {
		return nil, err
	}
	// filepath.Glob matches leading dots, whereas OpenSSH's include glob
	// requires a literal dot for each hidden path component.
	pattern := strings.Split(filepath.Clean(name), string(filepath.Separator))
	matches := files[:0]
	for _, file := range files {
		hidden := false
		for i, part := range strings.Split(filepath.Clean(file), string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") && !strings.HasPrefix(pattern[i], ".") {
				hidden = true
				break
			}
		}
		if !hidden {
			matches = append(matches, file)
		}
	}
	return matches, nil
}

// words handles quotes and the limited backslash escapes allowed by OpenSSH,
// preserving ordinary Windows path separators.
func words(s string) ([]string, error) {
	var result []string
	for i := 0; i < len(s); {
		if s[i] == ' ' || s[i] == '\t' {
			i++
			continue
		}
		if s[i] == '#' {
			break
		}
		var word strings.Builder
		var quote byte
		for i < len(s) {
			c := s[i]
			if quote == 0 && (c == ' ' || c == '\t') {
				break
			}
			i++
			switch {
			case c == '\\' && i < len(s) && (strings.ContainsRune(`\'"`, rune(s[i])) || (quote == 0 && s[i] == ' ')):
				word.WriteByte(s[i])
				i++
			case quote == 0 && (c == '"' || c == '\''):
				quote = c
			case quote != 0 && c == quote:
				quote = 0
			default:
				word.WriteByte(c)
			}
		}
		if quote != 0 {
			return nil, errors.New("unclosed quote")
		}
		result = append(result, word.String())
	}
	return result, nil
}
