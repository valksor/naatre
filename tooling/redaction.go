package tooling

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const Redacted = "<redacted>"

var (
	urlPattern           = regexp.MustCompile(`https?://[^\s"'<>]+`)
	authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(bearer|basic)\s+[^\s,"']+`)
	cookiePattern        = regexp.MustCompile(`(?i)((?:cookie|set-cookie)\s*[:=]\s*)[^\r\n"']+`)
	credentialPattern    = regexp.MustCompile(`(?i)((?:x-api-key|api[_-]?key|x-auth-token|auth[_-]?token|access[_-]?token|refresh[_-]?token|client[_-]?secret|password|passwd|secret)\s*[:=]\s*)[^\s,&;"']+`)
)

// RedactCredentials sanitizes saved history, URLs, logs, and exported snippets.
// It is the mandatory default boundary shared with the playground owned by #91.
func RedactCredentials(input string) string {
	if redacted, ok := redactJSON(input); ok {
		input = redacted
	}
	input = authorizationPattern.ReplaceAllString(input, `${1}${2} `+Redacted)
	input = cookiePattern.ReplaceAllString(input, `${1}`+Redacted)
	input = credentialPattern.ReplaceAllString(input, `${1}`+Redacted)
	return urlPattern.ReplaceAllStringFunc(input, redactURL)
}

// RedactAndBound applies the shared credential redactor before enforcing a
// byte limit. The returned value is always valid UTF-8 and never exceeds
// maxBytes, including its truncation marker.
func RedactAndBound(input string, maxBytes int) (string, bool) {
	redacted := RedactCredentials(input)
	if maxBytes <= 0 {
		return "", redacted != ""
	}
	if len(redacted) <= maxBytes {
		return redacted, false
	}
	const marker = "<truncated>"
	if maxBytes < len(marker) {
		return strings.Repeat(".", maxBytes), true
	}
	end := maxBytes - len(marker)
	for end > 0 && !utf8.ValidString(redacted[:end]) {
		end--
	}
	return redacted[:end] + marker, true
}

func redactJSON(input string) (string, bool) {
	var value any
	if json.Unmarshal([]byte(input), &value) != nil {
		return input, false
	}
	redactJSONValue(value)
	encoded, err := json.Marshal(value)
	return string(encoded), err == nil
}

func redactJSONValue(value any) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			redactJSONValue(child)
		}
	case map[string]any:
		for key, child := range typed {
			if IsCredentialName(key) {
				typed[key] = Redacted
				continue
			}
			redactJSONValue(child)
		}
	}
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Redacted
	}
	if parsed.User != nil {
		parsed.User = url.User(Redacted)
	}
	query := parsed.Query()
	for key := range query {
		if IsCredentialName(key) {
			query.Set(key, Redacted)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// IsCredentialName reports whether a header, object key, or query parameter
// belongs to the shared credential-redaction vocabulary.
func IsCredentialName(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "-", ""), "_", ""))
	switch normalized {
	case "authorization", "authtoken", "accesstoken", "refreshtoken", "token", "apikey", "api key", "clientsecret", "password", "passwd", "secret", "cookie", "setcookie":
		return true
	default:
		return false
	}
}
