package protocol

import (
	"regexp"
	"strings"
)

var (
	extensionIDPattern = regexp.MustCompile(`^(?:[a-z](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z](?:[a-z0-9-]*[a-z0-9])?$`)
	semverPattern      = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

// ValidExtensionID reports whether id is a non-reserved reverse-DNS
// extension identifier.
func ValidExtensionID(id string) bool {
	return extensionIDPattern.MatchString(id) && !strings.HasPrefix(id, "core.") && !strings.HasPrefix(id, "naatre.")
}

// ValidSemanticVersion reports whether version is a SemVer 2.0.0 version
// without a leading v.
func ValidSemanticVersion(version string) bool {
	if !semverPattern.MatchString(version) {
		return false
	}
	withoutBuild := strings.SplitN(version, "+", 2)[0]
	parts := strings.SplitN(withoutBuild, "-", 2)
	if len(parts) != 2 {
		return true
	}
	for _, identifier := range strings.Split(parts[1], ".") {
		numeric := true
		for _, char := range identifier {
			if char < '0' || char > '9' {
				numeric = false
				break
			}
		}
		if numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}
