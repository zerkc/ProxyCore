package renderers

import "strings"

// localeCompare approximates JavaScript's String.prototype.localeCompare for
// the identifiers the renderers sort (DNS names, record types, JSON text):
// letters compare case-insensitively first, and on a tie lowercase sorts before
// uppercase, as in ICU's default collation. Returns -1, 0 or 1.
func localeCompare(left, right string) int {
	if primary := strings.Compare(strings.ToLower(left), strings.ToLower(right)); primary != 0 {
		return primary
	}
	// Equal ignoring case: the first differing rune decides, lowercase first.
	leftRunes, rightRunes := []rune(left), []rune(right)
	for index := range leftRunes {
		if leftRunes[index] == rightRunes[index] {
			continue
		}
		if leftRunes[index] > rightRunes[index] {
			return -1
		}
		return 1
	}
	return 0
}
