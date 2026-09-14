package tooling

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
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
			if sensitiveName(key) {
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
		if sensitiveName(key) {
			query.Set(key, Redacted)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func sensitiveName(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "-", ""), "_", ""))
	switch normalized {
	case "authorization", "authtoken", "accesstoken", "refreshtoken", "token", "apikey", "api key", "clientsecret", "password", "passwd", "secret", "cookie", "setcookie":
		return true
	default:
		return false
	}
}
