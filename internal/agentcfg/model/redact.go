package model

import (
	"net/url"
	"regexp"
	"strings"
)

const redacted = "REDACTED"

const credentialName = `[A-Za-z0-9_-]*(?:authorization|bearer|token|secret|passw|api[-_]?key|cookie|credential|private[-_]?key)[A-Za-z0-9_-]*`

var (
	// urlPattern runs to the next quote, so URLs inside a JSON argument are
	// matched one by one while brackets and commas stay part of them.
	urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)
	// assignmentPattern is a credential name followed by ":" or "=", as in
	// "Authorization: Bearer x", "api_key=x", "--token=x" or a JSON property.
	// Everything after it, to the end of the argument, is its value.
	assignmentPattern = regexp.MustCompile(`(?is)(` + credentialName + `)(["']?\s*[:=]\s*)(.*)$`)
	// pendingPattern is a credential name whose value is the next argument:
	// a flag such as --token, or a name ending in ":" or "=".
	pendingPattern = regexp.MustCompile(`(?i)(?:^--?` + credentialName + `|` + credentialName + `["']?\s*[:=])$`)
	braced         = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	bare           = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z0-9_]*$`)
)

// redactEmbeddedURL redacts a URL found inside an argument. A match that
// holds a second URL, as in a comma-separated list, cannot be split reliably
// and is replaced whole.
func redactEmbeddedURL(match string) string {
	if strings.Count(match, "://") > 1 {
		return redacted
	}
	return RedactURL(match)
}

// RedactURL removes the user information and query values of a URL. A value
// that does not parse as a URL is replaced whole.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redacted
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	if u.RawQuery != "" {
		query := u.Query()
		for key := range query {
			query[key] = []string{redacted}
		}
		u.RawQuery = query.Encode()
	}
	return u.String()
}

// Redact removes what may be a credential from a command line before it is
// reported. The line is split into arguments as a POSIX shell would. In each
// argument, the user information and query of URLs are replaced, and so is
// everything after the first credential name ("Authorization: Bearer x",
// "api_key=x", a JSON property), to the end of the argument; the argument
// after a credential flag ("--token x") is replaced whole, including flags
// inside a quoted script. A variable reference holds no value and is kept.
func Redact(line string) string {
	var b strings.Builder
	last := 0
	// pending is set while the next argument is a credential's value.
	pending := false
	for _, arg := range splitArgs(line) {
		value := arg.value
		wasPending := pending
		pending = pendingPattern.MatchString(value)
		switch {
		case wasPending:
			if !arg.reference(value) {
				value = redacted
			}
		default:
			value = urlPattern.ReplaceAllStringFunc(value, redactEmbeddedURL)
			if m := assignmentPattern.FindStringSubmatchIndex(value); m != nil {
				if rest := value[m[6]:]; rest != "" && !arg.reference(rest) {
					value = value[:m[6]] + redacted
				}
			}
			// What remains of a quoted script, as in sh -c '...', can still
			// hold a credential flag.
			if strings.ContainsAny(value, " \t\n") {
				value = Redact(value)
			}
		}
		if value == arg.value {
			continue
		}
		b.WriteString(line[last:arg.start])
		b.WriteString(quote(value))
		last = arg.end
	}
	b.WriteString(line[last:])
	return b.String()
}

type argument struct {
	value      string
	start, end int
	// literal is set when part of the argument was single-quoted or escaped,
	// so a $ in it may not be expanded by the shell.
	literal bool
}

// reference reports whether value is a variable reference rather than a
// value: ${NAME}, or $NAME where the shell would expand it.
func (a argument) reference(value string) bool {
	return braced.MatchString(value) || (!a.literal && bare.MatchString(value))
}

// splitArgs splits a line into arguments with POSIX shell quoting: single
// quotes keep everything, double quotes and a bare backslash escape.
func splitArgs(line string) []argument {
	var args []argument
	var current strings.Builder
	start, inArg, literal := 0, false, false
	var quoteChar byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoteChar == '\'':
			if c == '\'' {
				quoteChar = 0
			} else {
				current.WriteByte(c)
			}
			continue
		case quoteChar == '"':
			switch {
			case c == '"':
				quoteChar = 0
			case c == '\\' && i+1 < len(line) && line[i+1] == '\n':
				i++ // line continuation
			case c == '\\' && i+1 < len(line) && strings.IndexByte("\"\\$`", line[i+1]) >= 0:
				i++
				literal = true
				current.WriteByte(line[i])
			default:
				current.WriteByte(c)
			}
			continue
		case c == '\\' && i+1 < len(line) && line[i+1] == '\n':
			i++ // line continuation
			continue
		case c == ' ' || c == '\t' || c == '\n':
			if inArg {
				args = append(args, argument{current.String(), start, i, literal})
				current.Reset()
				inArg = false
			}
			continue
		}
		if !inArg {
			start, inArg, literal = i, true, false
		}
		switch {
		case c == '\'':
			quoteChar, literal = c, true
		case c == '"':
			quoteChar = c
		case c == '\\' && i+1 < len(line):
			i++
			literal = true
			current.WriteByte(line[i])
		default:
			current.WriteByte(c)
		}
	}
	if inArg {
		args = append(args, argument{current.String(), start, len(line), literal})
	}
	return args
}

// quote writes an argument back so a shell would read it as one.
func quote(value string) string {
	if strings.ContainsAny(value, " \t\n'\"\\$`&;|<>()") {
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	}
	return value
}
