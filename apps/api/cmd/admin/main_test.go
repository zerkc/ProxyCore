package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunResetPasswordOutputAndArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := ""
	code := run(context.Background(), []string{"reset-password", " Owner "}, &stdout, &stderr, func(_ context.Context, username string) (string, error) {
		called = username
		return "one-time-secret", nil
	})
	if code != 0 || called != " Owner " || stdout.String() != "one-time-secret\n" || stdout.String() == "one-time-secret\none-time-secret\n" { t.Fatalf("code=%d called=%q stdout=%q stderr=%q", code, called, stdout.String(), stderr.String()) }
	if strings.Contains(stderr.String(), "one-time-secret") { t.Fatal("secret leaked to stderr") }

	stdout.Reset(); stderr.Reset()
	if code := run(context.Background(), []string{"reset-password"}, &stdout, &stderr, nil); code != 2 || stdout.Len() != 0 { t.Fatalf("usage code=%d stdout=%q", code, stdout.String()) }

	stdout.Reset(); stderr.Reset()
	if code := run(context.Background(), []string{"reset-password", "missing"}, &stdout, &stderr, func(context.Context, string) (string, error) { return "", errors.New("user not found") }); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "user not found") { t.Fatalf("error code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String()) }
}
