// Package logging holds the log file handling shared by every Pangolin
// process: secret redaction, size-based rotation that survives several
// processes sharing one file, and the writer behind the newt logger.
package logging

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Redacted replaces a secret value in a log line.
const Redacted = "[REDACTED]"

// minSecretLen is the shortest registered secret that is scrubbed by value;
// anything shorter would blank out ordinary words in the log.
const minSecretLen = 8

// maxRegisteredSecrets bounds the registry, which lives as long as the process.
const maxRegisteredSecrets = 64

// secretKey matches the name of a field whose value must not be logged:
// anything containing secret, token, password, ... plus a few exact names.
const secretKey = `(?:[A-Za-z0-9_]*(?:secret|token|password|passwd|passphrase|apikey|api_key|privatekey|private_key|presharedkey|preshared_key|serialnumber|serial_number)[A-Za-z0-9_]*|serial|psk|authorization|cookie)`

var (
	// "secret":"value" as it appears in JSON.
	jsonStringRe = regexp.MustCompile(`(?i)("` + secretKey + `"\s*:\s*")((?:[^"\\]|\\.)*)(")`)
	// "secret":12345 or "secret":["a","b"], JSON values that aren't strings.
	jsonRawRe = regexp.MustCompile(`(?i)("` + secretKey + `"\s*:\s*)(-?[0-9][0-9.eE+-]*|\[[^\]]*\])`)
	// \"secret\":\"value\", the same JSON inside a %q-quoted string. In the
	// value, \\\" is an escaped quote, \\\\ an escaped backslash and \\x any other
	// escape; a lone \" ends the value.
	jsonQuotedRe = regexp.MustCompile(`(?i)(\\"` + secretKey + `\\"\s*:\s*\\")((?:\\\\\\"|\\\\\\\\|\\\\[^"\\]|[^"\\])*)(\\")`)
	// Authorization: Bearer value
	authHeaderRe = regexp.MustCompile(`(?i)(\bauthorization\s*[:=]\s*)(?:(?:bearer|basic|token)\s+)?[^\s",;]+`)
	// Bearer value
	bearerRe = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9\-._~+/]{8,}=*`)
	// password: "two words" and password='two words': a quoted value is masked whole.
	pairDoubleQuotedRe = regexp.MustCompile(`(?i)\b(` + secretKey + `)(\s*[:=]\s*)"((?:[^"\\]|\\.)*)"`)
	pairSingleQuotedRe = regexp.MustCompile(`(?i)\b(` + secretKey + `)(\s*[:=]\s*)'((?:[^'\\]|\\.)*)'`)
	// Secret:value (Go %+v), secret=value, userToken: value, map[token:value]
	pairRe = regexp.MustCompile(`(?i)\b(` + secretKey + `)(\s*[:=]\s*)()([^\s"',;})\]&]+)`)
)

var nextFieldRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*:`)

// harmlessValues are what a flag or an empty field prints as. They carry no
// secret, and keeping them leaves "hasOlmSecret=true" readable.
var harmlessValues = map[string]bool{
	"true": true, "false": true, "nil": true, "<nil>": true, "null": true,
	"none": true, "unset": true, "empty": true, "set": true, "present": true,
	"missing": true, "absent": true, "0": true, "(nil)": true, "<empty>": true,
	"[set]": true, "[unset]": true,
}

var registry struct {
	mu       sync.RWMutex
	values   []string
	replacer *strings.Replacer
}

// RegisterSecrets makes Redact scrub these exact values wherever they show up,
// whatever the surrounding format. Use it for credentials a process holds,
// like the olm secret and the session token. Values shorter than eight
// characters are ignored.
func RegisterSecrets(values ...string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	changed := false
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen || containsString(registry.values, v) {
			continue
		}
		if len(registry.values) >= maxRegisteredSecrets {
			registry.values = registry.values[1:]
		}
		registry.values = append(registry.values, v)
		changed = true
	}
	if !changed {
		return
	}
	// Longest first, so a secret that contains another is replaced whole.
	sorted := append([]string(nil), registry.values...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	pairs := make([]string, 0, len(sorted)*2)
	for _, v := range sorted {
		pairs = append(pairs, v, Redacted)
	}
	registry.replacer = strings.NewReplacer(pairs...)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Redact masks credentials and hardware serials in a log line: values of
// fields named like a secret, token, password or serial (JSON, key=value,
// Go struct dumps), bearer tokens, and any value given to RegisterSecrets.
// It is idempotent.
func Redact(s string) string {
	if s == "" {
		return s
	}
	registry.mu.RLock()
	replacer := registry.replacer
	registry.mu.RUnlock()
	if replacer != nil {
		s = replacer.Replace(s)
	}

	// Cheap exit: none of the patterns can match without one of these.
	lower := strings.ToLower(s)
	if !containsAny(lower, "secret", "token", "password", "passwd", "passphrase", "apikey", "api_key", "serial", "authorization", "cookie", "bearer", "private", "psk") {
		return s
	}

	s = jsonQuotedRe.ReplaceAllString(s, "${1}"+Redacted+"${3}")
	s = jsonStringRe.ReplaceAllString(s, "${1}"+Redacted+"${3}")
	s = jsonRawRe.ReplaceAllString(s, `${1}"`+Redacted+`"`)
	s = pairDoubleQuotedRe.ReplaceAllStringFunc(s, func(m string) string { return redactQuoted(pairDoubleQuotedRe, m, `"`) })
	s = pairSingleQuotedRe.ReplaceAllStringFunc(s, func(m string) string { return redactQuoted(pairSingleQuotedRe, m, `'`) })
	s = authHeaderRe.ReplaceAllString(s, "${1}"+Redacted)
	s = bearerRe.ReplaceAllString(s, "${1}"+Redacted)
	s = pairRe.ReplaceAllStringFunc(s, redactPair)
	return s
}

// weakKeyRe matches the key names that also show up in ordinary messages
// ("Failed to open thread token: Access is denied"). Their values are only
// masked when they look like a credential. Any other key, like userToken or
// olmSecret, always has its value masked.
var weakKeyRe = regexp.MustCompile(`(?i)^(?:token|serial)$`)

func looksLikeCredential(v string) bool {
	if len(v) < minSecretLen {
		return false
	}
	return len(v) >= 24 || strings.ContainsAny(v, "0123456789._-+/=")
}

func redactQuoted(re *regexp.Regexp, match, quote string) string {
	m := re.FindStringSubmatch(match)
	if m == nil {
		return match
	}
	value := m[3]
	if value == "" || harmlessValues[strings.ToLower(value)] || strings.HasPrefix(value, "[REDACTED") {
		return match
	}
	if weakKeyRe.MatchString(m[1]) && !looksLikeCredential(value) {
		return match
	}
	return m[1] + m[2] + quote + Redacted + quote
}

func redactPair(match string) string {
	m := pairRe.FindStringSubmatch(match)
	if m == nil {
		return match
	}
	value := m[4]
	if harmlessValues[strings.ToLower(value)] || strings.HasPrefix(value, "[REDACTED") {
		return match
	}
	// An empty field in a Go struct dump ("Secret: UserToken:x") is followed
	// by the next field's name, not by a value.
	if strings.TrimSpace(m[2]) != m[2] && nextFieldRe.MatchString(value) {
		return match
	}
	if weakKeyRe.MatchString(m[1]) && !looksLikeCredential(value) {
		return match
	}
	return m[1] + m[2] + m[3] + Redacted
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
