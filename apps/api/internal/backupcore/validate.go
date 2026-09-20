package backupcore

import (
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

func Validate(m Manifest) error {
	if len(m.unknownTopLevelKeys) > 0 {
		return fmt.Errorf("unknown top-level manifest key %q", m.unknownTopLevelKeys[0])
	}
	if len(m.duplicateTopLevelKeys) > 0 {
		return fmt.Errorf("duplicate top-level manifest key %q", m.duplicateTopLevelKeys[0])
	}
	if len(m.invalidTopLevelFields) > 0 {
		return fmt.Errorf("invalid top-level manifest field %q", m.invalidTopLevelFields[0])
	}
	if m.envPresentMissing {
		return fmt.Errorf("missing required manifest key %q", "envPresent")
	}
	if m.FormatVersion == "" {
		return fmt.Errorf("missing required manifest key %q", "formatVersion")
	}
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported manifest format version %q", m.FormatVersion)
	}
	if m.CreatedAt == "" {
		return fmt.Errorf("missing required manifest key %q", "createdAt")
	}
	createdAt, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil || createdAt.Location() != time.UTC {
		return fmt.Errorf("createdAt must be an RFC 3339 UTC timestamp")
	}
	if m.ExporterVersion == "" {
		return fmt.Errorf("missing required manifest key %q", "exporterVersion")
	}
	if m.InstallationID == "" {
		return fmt.Errorf("missing required manifest key %q", "installationId")
	}
	if m.NodeID == "" {
		return fmt.Errorf("missing required manifest key %q", "nodeId")
	}
	if m.Role == "" {
		return fmt.Errorf("missing required manifest key %q", "role")
	}
	if m.EntryChecksums == nil {
		return fmt.Errorf("missing required manifest key %q", "entryChecksums")
	}
	if m.DBTables == nil {
		return fmt.Errorf("missing required manifest key %q", "dbTables")
	}
	if len(m.duplicateChecksumPaths) > 0 {
		return fmt.Errorf("duplicate entry checksum path %q", m.duplicateChecksumPaths[0])
	}
	for entryPath, checksum := range m.EntryChecksums {
		if escapesBundleRoot(entryPath) {
			return fmt.Errorf("entry checksum path escapes bundle root: %q", entryPath)
		}
		if len(checksum) != 64 {
			return fmt.Errorf("entry checksum for %q is not a SHA-256 hex digest", entryPath)
		}
		if _, err := hex.DecodeString(checksum); err != nil {
			return fmt.Errorf("entry checksum for %q is not a SHA-256 hex digest", entryPath)
		}
	}
	for _, certPath := range m.Certs {
		if escapesBundleRoot(certPath) {
			return fmt.Errorf("certificate path escapes bundle root: %q", certPath)
		}
	}
	return nil
}

func escapesBundleRoot(value string) bool {
	if value == "" || path.IsAbs(value) || strings.Contains(value, `\`) {
		return true
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}
