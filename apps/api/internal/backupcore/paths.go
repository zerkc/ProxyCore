package backupcore

import (
	"path"
	"strings"
)

// EscapesBundleRoot reports whether value is not a safe slash-separated path
// relative to an archive or bundle root.
func EscapesBundleRoot(value string) bool {
	if value == "" || path.IsAbs(value) || strings.Contains(value, `\`) || containsWindowsDriveRoot(value) {
		return true
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func containsWindowsDriveRoot(value string) bool {
	for index := 0; index+2 < len(value); index++ {
		if index > 0 && value[index-1] != '/' {
			continue
		}
		letter := value[index]
		if ((letter >= 'a' && letter <= 'z') || (letter >= 'A' && letter <= 'Z')) &&
			value[index+1] == ':' && (value[index+2] == '/' || value[index+2] == '\\') {
			return true
		}
	}
	return false
}
