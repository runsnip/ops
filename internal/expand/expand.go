// Package expand substitutes variables the way docker compose and POSIX sh do in the forms a stack file uses:
// $NAME, ${NAME}, ${NAME:-default} (default when unset or empty), ${NAME-default} (default when unset) and
// ${NAME:+alt}; defaults nest (${A:-${B}}). Unknown forms are left as written.
package expand

import "strings"

// Lookup returns a variable's value and whether it is set.
type Lookup func(name string) (string, bool)

// Map is a Lookup over a map.
func Map(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// String expands s.
func String(s string, look Lookup) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if s[i+1] == '$' { // $$ is a literal $ in compose
			b.WriteByte('$')
			i += 2
			continue
		}
		if s[i+1] == '{' {
			end := matchBrace(s, i+1)
			if end < 0 {
				b.WriteString(s[i:])
				break
			}
			b.WriteString(braced(s[i+2:end], look))
			i = end + 1
			continue
		}
		j := i + 1
		for j < len(s) && isName(s[j], j == i+1) {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			i++
			continue
		}
		v, _ := look(s[i+1 : j])
		b.WriteString(v)
		i = j
	}
	return b.String()
}

func isName(c byte, first bool) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || !first && c >= '0' && c <= '9'
}

// matchBrace finds the } closing the { at open, counting nested ${…}.
func matchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func braced(body string, look Lookup) string {
	n := 0
	for n < len(body) && isName(body[n], n == 0) {
		n++
	}
	name, rest := body[:n], body[n:]
	v, set := look(name)
	switch {
	case rest == "":
		return v
	case strings.HasPrefix(rest, ":-"):
		if v == "" {
			return String(rest[2:], look)
		}
		return v
	case strings.HasPrefix(rest, "-"):
		if !set {
			return String(rest[1:], look)
		}
		return v
	case strings.HasPrefix(rest, ":+"):
		if v != "" {
			return String(rest[2:], look)
		}
		return ""
	case strings.HasPrefix(rest, ":?"), strings.HasPrefix(rest, "?"):
		return v
	}
	return "${" + body + "}"
}

// Names lists the variables s refers to (defaults included), in order, once each.
func Names(s string) []string {
	var out []string
	seen := map[string]bool{}
	String(s, func(k string) (string, bool) {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
		return "", false
	})
	return out
}
