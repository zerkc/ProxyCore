package configuration

import (
	"strings"
	"testing"
)

func TestNormalizeEnrollmentHostnamesCanonicalizesAndSorts(t *testing.T) {
	got, err := NormalizeEnrollmentHostnames([]string{
		"Example.COM.",
		"2001:0DB8:0:0:0:0:0:1",
		"192.0.2.1",
		"example.com",
		"192.0.2.1",
	})
	if err != nil {
		t.Fatalf("normalize enrollment hostnames: %v", err)
	}
	want := []string{"192.0.2.1", "2001:db8::1", "example.com"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized=%v, want %v", got, want)
	}
}

func TestNormalizeEnrollmentHostnamesRejectsNonExactValues(t *testing.T) {
	for _, value := range []string{
		"*.example.com",
		"https://example.com",
		"example.com:3443",
		"192.0.2.0/24",
		"user@example.com",
		"example.com/path",
		"example.com?query",
		"example.com#fragment",
		"[2001:db8::1]",
		"example.com..",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := NormalizeEnrollmentHostnames([]string{value}); err == nil {
				t.Fatalf("accepted non-exact enrollment hostname %q", value)
			}
		})
	}
}

func TestNormalizeEnrollmentHostnamesEnforcesBounds(t *testing.T) {
	tooMany := make([]string, MaxEnrollmentHostnames+1)
	for i := range tooMany {
		tooMany[i] = "host" + string(rune('a'+i%26)) + ".example.com"
	}
	if _, err := NormalizeEnrollmentHostnames(tooMany); err == nil {
		t.Fatal("accepted too many enrollment hostnames")
	}

	tooLong := strings.Repeat("a", MaxEnrollmentHostnameLength+1)
	if _, err := NormalizeEnrollmentHostnames([]string{tooLong}); err == nil {
		t.Fatal("accepted overlong enrollment hostname")
	}
}
