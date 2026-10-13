// Command convert is the offline, one-way PostgreSQL to SQLite converter. It
// reads a consistent read-only snapshot of an existing ProxyCore PostgreSQL
// database, writes a SQLite file, validates it independently and only then
// publishes it. PostgreSQL is never written.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/pgconvert"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

const (
	exitOK         = 0
	exitValidation = 1 // validation or fidelity failure, or an unexpected internal error
	exitUsage      = 2
	exitSource     = 3 // PostgreSQL unreachable, or schema/data the converter cannot map

	toolVersion  = "proxycore-convert/1"
	markerFormat = "proxycore-convert-marker/v1"
	markerSuffix = ".converted.json"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, getenv, stdout, stderr)
}

type options struct{ pgURL, out, masterKey string }

func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (options, bool) {
	var opts options
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `Usage: convert --pg-url URL --out FILE [--master-key-base64 KEY]

Converts a PostgreSQL ProxyCore database into a new SQLite file.
  --pg-url              PostgreSQL URL (or PROXYCORE_CONVERT_PG_URL); never logged
  --out                 SQLite file to create; it must not exist
  --master-key-base64   master key (or PROXYCORE_MASTER_KEY_BASE64); prefer the environment variable
Exit codes: 0 ok, 1 validation failure, 2 usage, 3 source schema/data error.`)
	}
	fs.StringVar(&opts.pgURL, "pg-url", "", "")
	fs.StringVar(&opts.out, "out", "", "")
	fs.StringVar(&opts.masterKey, "master-key-base64", "", "")
	if err := fs.Parse(args); err != nil {
		return opts, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "convert: unexpected argument %q\n", fs.Arg(0))
		return opts, false
	}
	if opts.pgURL == "" {
		opts.pgURL = getenv("PROXYCORE_CONVERT_PG_URL")
	}
	if opts.masterKey == "" {
		opts.masterKey = getenv("PROXYCORE_MASTER_KEY_BASE64")
	}
	switch {
	case opts.pgURL == "":
		fmt.Fprintln(stderr, "convert: --pg-url or PROXYCORE_CONVERT_PG_URL is required")
	case opts.out == "":
		fmt.Fprintln(stderr, "convert: --out is required")
	case opts.masterKey == "":
		fmt.Fprintln(stderr, "convert: --master-key-base64 or PROXYCORE_MASTER_KEY_BASE64 is required")
	default:
		if _, err := secrets.DecodeMasterKey(opts.masterKey); err != nil {
			fmt.Fprintln(stderr, "convert: the master key must be base64 that decodes to 32 bytes")
			return opts, false
		}
		return opts, true
	}
	return opts, false
}

func sidecars(path string) []string {
	return []string{path, path + "-wal", path + "-shm", path + "-journal"}
}

func removeAll(paths ...string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

func exists(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

// exitFor maps an error from the source or the copy to an exit code.
func exitFor(err error) int {
	var schemaErr *pgconvert.SchemaError
	var dataErr *pgconvert.DataError
	if errors.As(err, &schemaErr) || errors.As(err, &dataErr) {
		return exitSource
	}
	return exitValidation
}

func runContext(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	opts, ok := parseOptions(args, getenv, stderr)
	if !ok {
		return exitUsage
	}
	out, err := filepath.Abs(opts.out)
	if err != nil {
		fmt.Fprintf(stderr, "convert: %v\n", err)
		return exitUsage
	}
	if info, err := os.Stat(filepath.Dir(out)); err != nil || !info.IsDir() {
		fmt.Fprintln(stderr, "convert: the output directory does not exist")
		return exitUsage
	}
	tmp, marker := out+".tmp", out+markerSuffix
	for _, path := range append(append(sidecars(out), sidecars(tmp)...), marker, marker+".tmp") {
		if found, err := exists(path); err != nil {
			fmt.Fprintf(stderr, "convert: %v\n", err)
			return exitUsage
		} else if found {
			fmt.Fprintf(stderr, "convert: refusing to continue, %s already exists (nothing was modified)\n", filepath.Base(path))
			return exitUsage
		}
	}

	src, err := pgconvert.OpenSource(ctx, opts.pgURL)
	if err != nil {
		fmt.Fprintf(stderr, "convert: cannot open the PostgreSQL source: %s\n", redact(err, opts))
		return exitSource
	}
	defer src.Close(context.Background())

	published := false
	defer func() {
		if !published {
			removeAll(sidecars(tmp)...)
			removeAll(marker + ".tmp")
		}
	}()

	report, err := pgconvert.Convert(ctx, src, tmp)
	if err != nil {
		fmt.Fprintf(stderr, "convert: conversion failed: %s\n", redact(err, opts))
		return exitFor(err)
	}
	validation, err := pgconvert.Validate(ctx, src, tmp, opts.masterKey)
	if err != nil {
		fmt.Fprintf(stderr, "convert: validation failed, nothing was published: %s\n", redact(err, opts))
		var ve *pgconvert.ValidationError
		if errors.As(err, &ve) {
			return exitValidation
		}
		return exitFor(err)
	}
	fingerprint, err := src.SchemaFingerprint(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "convert: cannot fingerprint the source schema: %s\n", redact(err, opts))
		return exitFor(err)
	}
	removeAll(tmp+"-wal", tmp+"-shm", tmp+"-journal") // closed cleanly; belt and braces
	if err := os.Chmod(tmp, 0o600); err != nil {
		fmt.Fprintf(stderr, "convert: %v\n", err)
		return exitValidation
	}
	if err := syncPath(tmp); err != nil {
		fmt.Fprintf(stderr, "convert: fsync output: %v\n", err)
		return exitValidation
	}
	// Link then unlink is an atomic rename that can never overwrite a racing output.
	if err := os.Link(tmp, out); err != nil {
		fmt.Fprintf(stderr, "convert: publish failed (the output filesystem must support hard links): %v\n", err)
		return exitValidation
	}
	published = true
	_ = os.Remove(tmp)
	if err := syncPath(filepath.Dir(out)); err != nil {
		fmt.Fprintf(stderr, "convert: warning: directory fsync failed: %v\n", err)
	}
	if err := writeMarker(marker, fingerprint, report, validation); err != nil {
		// Without the marker the output is not provably converted and validated.
		removeAll(sidecars(out)...)
		removeAll(marker, marker+".tmp")
		fmt.Fprintf(stderr, "convert: cannot write the success marker, output removed: %v\n", err)
		return exitValidation
	}
	printSummary(stdout, out, marker, report, validation)
	return exitOK
}

// redact removes the connection string and master key from an error message.
func redact(err error, opts options) string {
	msg := err.Error()
	for _, secret := range []string{opts.pgURL, opts.masterKey} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return msg
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

type markerTable struct {
	Rows int64  `json:"rows"`
	Hash string `json:"hash"`
}

type markerWarning struct {
	Code    string   `json:"code"`
	Table   string   `json:"table"`
	Column  string   `json:"column,omitempty"`
	Count   int64    `json:"count"`
	Samples []string `json:"samples,omitempty"`
}

type markerGap struct {
	Table  string `json:"table"`
	Column string `json:"column"`
}

// marker proves "converted and validated". It carries no secrets: counts,
// canonical hashes, warning identifiers and the source schema fingerprint.
type marker struct {
	Format            string                 `json:"format"`
	ToolVersion       string                 `json:"tool_version"`
	ConvertedAt       string                 `json:"converted_at"`
	SchemaVersion     int                    `json:"schema_version"`
	SourceFingerprint string                 `json:"source_schema_fingerprint"`
	Tables            map[string]markerTable `json:"tables"`
	Warnings          []markerWarning        `json:"warnings"`
	Gaps              []markerGap            `json:"gaps"`
}

func writeMarker(path, fingerprint string, report *pgconvert.Report, validation *pgconvert.ValidationReport) error {
	m := marker{Format: markerFormat, ToolVersion: toolVersion, ConvertedAt: time.Now().UTC().Format(time.RFC3339),
		SchemaVersion: validation.SchemaVersion, SourceFingerprint: fingerprint,
		Tables: map[string]markerTable{}, Warnings: []markerWarning{}, Gaps: []markerGap{}}
	for name, t := range validation.Tables {
		m.Tables[name] = markerTable{Rows: t.DestRows, Hash: t.DestHash}
	}
	for _, w := range report.Warnings {
		m.Warnings = append(m.Warnings, markerWarning{Code: w.Code, Table: w.Table, Column: w.Column, Count: w.Count, Samples: w.Samples})
	}
	for _, g := range report.Gaps {
		m.Gaps = append(m.Gaps, markerGap{Table: g.Table, Column: g.Column})
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		return err
	}
	_ = os.Remove(tmp)
	return syncPath(filepath.Dir(path))
}

func printSummary(w io.Writer, out, marker string, report *pgconvert.Report, validation *pgconvert.ValidationReport) {
	names := make([]string, 0, len(validation.Tables))
	var total int64
	for name, t := range validation.Tables {
		names = append(names, name)
		total += t.DestRows
	}
	sort.Strings(names)
	fmt.Fprintf(w, "Converted %d tables, %d rows.\n", len(names), total)
	for _, name := range names {
		if rows := validation.Tables[name].DestRows; rows > 0 {
			fmt.Fprintf(w, "  %-32s %d\n", name, rows)
		}
	}
	fmt.Fprintf(w, "Output validated: row counts and canonical hashes match PostgreSQL; foreign_key_check and integrity_check clean;\n")
	fmt.Fprintf(w, "  schema version %d; %d secrets decrypted and byte-identical; %d revision checksums and %d snapshot bodies verified",
		validation.SchemaVersion, validation.SecretsDecrypted, validation.RevisionChecksums, validation.SnapshotBlobs)
	if validation.LegacySnapshots > 0 {
		fmt.Fprintf(w, " (%d legacy snapshots without a body)", validation.LegacySnapshots)
	}
	fmt.Fprintln(w, ".")
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "WARNING %s: %s.%s: %d rows (samples: %s)\n", warning.Code, warning.Table, warning.Column, warning.Count, strings.Join(warning.Samples, ", "))
	}
	for _, gap := range report.Gaps {
		fmt.Fprintf(w, "WARNING older source schema: %s.%s is missing; the default was written\n", gap.Table, gap.Column)
	}
	fmt.Fprintf(w, "Database: %s\nMarker:   %s\n", out, marker)
}
