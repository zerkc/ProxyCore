package configuration

import (
	"encoding/json"

	"github.com/google/uuid"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func zeroGrantBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

const installationIDSingleton = "default"

func zeroCanonicalPlainSecrets(values []replicationsnapshot.PlainSecret) {
	for index := range values {
		for byteIndex := range values[index].Value {
			values[index].Value[byteIndex] = 0
		}
		values[index].Value = nil
	}
}

func validPublicationUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func nullablePtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

type scanner interface {
	Scan(dest ...any) error
}

func rawJSON(value []byte) json.RawMessage {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return json.RawMessage(value)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

const installationID = "default"

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
