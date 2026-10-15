// Package pgconvert is the offline, one-way PostgreSQL to SQLite converter
// core. It is the only code that still reads PostgreSQL: the source is read
// inside one REPEATABLE READ READ ONLY transaction and never written.
package pgconvert

import "fmt"

// Kind is the PostgreSQL type family of a column and how it is stored in SQLite.
type Kind int

const (
	KindUUID      Kind = iota // uuid -> lowercase 36-char TEXT
	KindText                  // text -> TEXT, copied byte for byte
	KindJSON                  // jsonb -> TEXT, raw ::text (never decoded)
	KindInt                   // integer/bigint -> exact INTEGER
	KindBool                  // boolean -> INTEGER 0/1
	KindTimestamp             // timestamptz -> tagged TEXT (sqlitedb.EncodeTimestamp)
	KindBytes                 // bytea -> BLOB
	KindEnum                  // PostgreSQL enum -> TEXT, labels validated
)

func (k Kind) sqliteType() string {
	switch k {
	case KindInt, KindBool:
		return "INTEGER"
	case KindBytes:
		return "BLOB"
	default:
		return "TEXT"
	}
}

// ColumnSpec describes one destination column. Missing source columns are only
// tolerated when Older is set (documented older PostgreSQL variants), in which
// case Default (nil means SQL NULL) is written instead.
type ColumnSpec struct {
	Name       string
	Kind       Kind
	Nullable   bool
	Older      bool
	Default    any // nil, string or int64 (bool defaults are 0/1)
	EnumType   string
	EnumValues []string
}

func (c ColumnSpec) null() ColumnSpec         { c.Nullable = true; return c }
func (c ColumnSpec) older(def any) ColumnSpec { c.Older, c.Default = true, def; return c }
func (c ColumnSpec) olderNull() ColumnSpec {
	c.Nullable, c.Older, c.Default = true, true, nil
	return c
}

// TableSpec lists a table's columns in destination order; that order is also
// the canonical order used by row hashes.
type TableSpec struct {
	Name    string
	Columns []ColumnSpec
}

type enumSpec struct {
	typ    string
	values []string
}

func enumOf(typ string, values ...string) enumSpec { return enumSpec{typ, values} }

var (
	enumRole        = enumOf("proxycore_role", "owner", "operator")
	enumRecordType  = enumOf("proxycore_record_type", "A", "AAAA", "CNAME", "TXT", "MX", "SRV")
	enumStreamProto = enumOf("proxycore_stream_protocol", "tcp", "udp")
	enumIssuer      = enumOf("proxycore_certificate_issuer", "self-signed", "uploaded", "letsencrypt")
	enumChallenge   = enumOf("proxycore_certificate_challenge", "none", "http-01", "dns-01")
	enumCertStatus  = enumOf("proxycore_certificate_status", "pending", "issued", "active", "expired", "failed")
	enumJobStatus   = enumOf("proxycore_job_status", "queued", "validating", "applying", "applied", "failed", "rolled-back")
	enumJobTarget   = enumOf("proxycore_job_target", "coredns", "nginx", "combined", "certificate")
	enumSource      = enumOf("proxycore_persistence_source", "ordinary", "import", "sync")
	enumTopology    = enumOf("proxycore_topology_role", "standalone-primary", "primary", "primary-with-nodes", "node", "stale-primary")
	enumSnapshot    = enumOf("proxycore_applied_snapshot_status", "pending", "applied", "rejected", "rolled-back", "archived")
	enumAttempt     = enumOf("proxycore_enrollment_attempt_state", "draft", "verified", "confirmed", "exchanged", "archived", "initial-apply-pending", "committed", "cancelled", "recoverable", "failed")
	enumSyncTrigger = enumOf("proxycore_sync_trigger", "enrollment", "periodic", "manual", "restart")
	enumSyncStatus  = enumOf("proxycore_sync_attempt_status", "queued", "running", "current", "applied", "failed", "pending-ack")
)

func uuid(n string) ColumnSpec  { return ColumnSpec{Name: n, Kind: KindUUID} }
func text(n string) ColumnSpec  { return ColumnSpec{Name: n, Kind: KindText} }
func jsonb(n string) ColumnSpec { return ColumnSpec{Name: n, Kind: KindJSON} }
func num(n string) ColumnSpec   { return ColumnSpec{Name: n, Kind: KindInt} }
func flag(n string) ColumnSpec  { return ColumnSpec{Name: n, Kind: KindBool} }
func ts(n string) ColumnSpec    { return ColumnSpec{Name: n, Kind: KindTimestamp} }
func blob(n string) ColumnSpec  { return ColumnSpec{Name: n, Kind: KindBytes} }
func enum(n string, e enumSpec) ColumnSpec {
	return ColumnSpec{Name: n, Kind: KindEnum, EnumType: e.typ, EnumValues: e.values}
}

// provenance lists the persistence-source columns added by Drizzle 0004.
func provenance() []ColumnSpec {
	return []ColumnSpec{
		enum("source", enumSource).older("ordinary"),
		uuid("source_primary_id").olderNull(), uuid("source_node_id").olderNull(), uuid("source_revision_id").olderNull(),
		text("snapshot_content_hash").olderNull(), num("snapshot_version").olderNull(),
		num("replication_version").olderNull(), num("leadership_generation").olderNull(),
	}
}

func join(groups ...[]ColumnSpec) []ColumnSpec {
	var all []ColumnSpec
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

func cols(c ...ColumnSpec) []ColumnSpec { return c }

// tables is the single explicit mapping: table, ordered columns and kinds, in
// foreign-key-safe copy order (every FK parent precedes its children). The
// three FKs dropped by Drizzle 0007 (enrollment_tokens.consumed_by_attempt_id,
// enrollment_grants.attempt_id, enrolled_nodes.created_by_attempt_id) are plain
// columns here; dangling values are reported as warnings.
var tables = []TableSpec{
	{"users", cols(uuid("id"), text("username"), text("password_hash"), enum("role", enumRole), flag("active"),
		flag("password_change_required").older(int64(0)), ts("created_at"), ts("updated_at"))},
	{"sessions", cols(uuid("id"), uuid("user_id"), text("token_hash"), ts("expires_at"), ts("revoked_at").null(),
		ts("last_seen_at").null(), ts("created_at"))},
	{"audit_events", cols(uuid("id"), uuid("actor_user_id").null(), text("action"), text("resource_type"),
		text("resource_id").null(), jsonb("before_value").null(), jsonb("after_value").null(),
		text("correlation_id"), text("result"), ts("created_at"))},
	{"installation_settings", cols(text("id"), text("ingress_ipv4").null(), text("ingress_ipv6").null(),
		jsonb("default_resolver_pool").null(), jsonb("forwarding_rules").null(), jsonb("enrollment_hostnames").olderNull(),
		num("retention_max_age_days"), num("retention_max_size_mb"), text("current_desired_revision_id").null(),
		text("current_applied_revision_id").null(), ts("created_at"), ts("updated_at"))},
	{"zones", cols(uuid("id"), text("name"), flag("enabled"), ts("created_at"), ts("updated_at"))},
	{"dns_records", cols(uuid("id"), uuid("zone_id"), text("name"), enum("type", enumRecordType), jsonb("value"),
		num("ttl"), flag("enabled"), flag("proxied"), jsonb("proxy_settings").null(), text("comment").null(),
		ts("created_at"), ts("updated_at"))},
	{"resolver_pools", cols(uuid("id"), text("name"), jsonb("endpoints"), flag("is_default"), ts("created_at"), ts("updated_at"))},
	{"forwarding_rules", cols(uuid("id"), text("suffix"), uuid("pool_id"), ts("created_at"), ts("updated_at"))},
	{"stream_routes", cols(uuid("id"), flag("enabled"), enum("protocol", enumStreamProto), text("listen_address"),
		num("listen_port"), jsonb("upstream"), ts("created_at"), ts("updated_at"))},
	{"secrets", cols(uuid("id"), text("purpose"), text("ciphertext"), ts("created_at"), ts("updated_at"))},
	{"certificates", cols(uuid("id"), jsonb("hostnames"), enum("issuer", enumIssuer), enum("challenge", enumChallenge),
		text("environment"), enum("status", enumCertStatus), ts("expires_at").null(), ts("renew_after").null(),
		uuid("key_secret_id").null(), text("certificate_pem").null(), ts("created_at"), ts("updated_at"),
		text("failure_reason").olderNull())},
	{"internal_ca", cols(text("id"), text("certificate_pem"), uuid("key_secret_id"), ts("created_at"), ts("updated_at"),
		text("enrollment_certificate_pem").olderNull(), uuid("enrollment_key_secret_id").olderNull())},
	{"provider_connections", cols(uuid("id"), text("provider"), text("name"), uuid("secret_id"), text("scope"),
		flag("enabled"), ts("created_at"), ts("updated_at"))},
	{"config_revisions", join(cols(uuid("id"), num("revision_number"), text("checksum"), jsonb("snapshot"),
		uuid("actor_user_id").null(), ts("created_at"), ts("applied_at").null()), provenance())},
	{"apply_jobs", join(cols(uuid("id"), uuid("revision_id"), uuid("actor_user_id").null(), enum("target", enumJobTarget),
		enum("status", enumJobStatus), text("correlation_id"), jsonb("validation_output").null(),
		jsonb("apply_output").null(), jsonb("health_output").null(), text("error_message").null(),
		ts("claimed_at").null(), ts("started_at").null(), ts("finished_at").null(), ts("created_at")), provenance())},
	{"health_observations", cols(uuid("id"), text("component"), text("status"), jsonb("details").null(), ts("created_at"))},
	{"operational_artifacts", cols(uuid("id"), text("kind"), text("path"), num("size_bytes"),
		uuid("revision_id").null(), ts("created_at"))},
	{"installation_identity", cols(text("id"), uuid("installation_id"), uuid("node_id"), enum("role", enumTopology),
		num("leadership_generation"), num("latest_known_generation"), uuid("cluster_key_id").null(), ts("updated_at"))},
	{"cluster_keys", cols(uuid("id"), text("purpose"), text("wrapped_kek"), num("wrapping_key_version"),
		ts("created_at"), ts("retired_at").null())},
	{"node_state", cols(text("id"), ts("enrolled_at").null(), text("enrollment_token_hash").null(),
		uuid("enrollment_primary_id").null(), ts("last_seen_at").null(), uuid("last_applied_snapshot_id").null(),
		uuid("enrollment_attempt_id").olderNull(), text("primary_url").olderNull(), uuid("primary_installation_id").olderNull(),
		text("primary_tls_spki_sha256").olderNull(), uuid("credential_id").olderNull(), flag("sync_enabled").older(int64(0)),
		ts("last_attempt_at").olderNull(), ts("last_success_at").olderNull(), num("consecutive_failures").older(int64(0)),
		ts("next_attempt_at").olderNull(), text("last_error_code").olderNull(), ts("updated_at"))},
	{"applied_snapshots", cols(uuid("id"), uuid("source_primary_id"), num("leadership_generation"),
		num("snapshot_version"), num("replication_version"), text("content_hash"), uuid("revision_id").null(),
		enum("status", enumSnapshot).older("applied"), uuid("apply_job_id").olderNull(), text("failure_code").olderNull(),
		ts("applied_at"), ts("discarded_at").null(), blob("snapshot_body").olderNull())},
	{"standalone_archives", cols(uuid("id"), text("snapshot_content_hash").null(), uuid("revision_id").null(),
		text("archive_blob"), text("capture_reason"), ts("captured_at"), ts("retention_started_at").null(),
		ts("expires_at").null(), flag("restoration_eligible"), ts("created_at"))},
	{"enrollment_tokens", cols(uuid("id"), text("token_selector"), text("token_hash"), text("hash_version"),
		uuid("created_by_user_id"), ts("created_at"), ts("expires_at"), ts("consumed_at").null(),
		uuid("consumed_by_attempt_id").null(), ts("revoked_at").null())},
	{"enrollment_grants", cols(uuid("attempt_id"), uuid("token_id"), uuid("installation_id"), uuid("node_id"),
		uuid("primary_id"), num("primary_generation"), text("node_ephemeral_public_key"), text("sealed_bootstrap_payload"),
		text("payload_hash"), ts("created_at"), ts("expires_at"), text("verified_preview_digest").olderNull())},
	{"enrollment_attempts", cols(uuid("id"), enum("state", enumAttempt), text("primary_url"),
		uuid("expected_primary_id").null(), uuid("verified_primary_id").null(), uuid("verified_primary_node_id").null(),
		num("verified_leadership_generation").null(), text("verified_primary_tls_spki_sha256").null(),
		text("verified_primary_ca_fingerprint").null(), text("preview_digest").null(), text("local_node_ip"),
		uuid("archive_id").null(), text("ephemeral_private_key_wrapped"), text("bootstrap_payload").null(),
		uuid("node_credential_secret_id").null(), uuid("cluster_key_id").null(), text("initial_snapshot_hash").null(),
		uuid("initial_snapshot_revision_id").null(), uuid("initial_apply_job_id").null(), text("failure_code").null(),
		ts("confirmed_at").null(), ts("created_at"), ts("updated_at"))},
	{"node_credentials", cols(uuid("id"), uuid("node_id"), text("credential_hash"), text("hash_version"),
		ts("created_at"), ts("last_authenticated_at").null(), ts("revoked_at").null())},
	{"enrolled_nodes", cols(uuid("node_id"), uuid("installation_id"), uuid("primary_id"), text("display_name").null(),
		uuid("credential_id"), ts("enrolled_at"), ts("revoked_at").null(), uuid("created_by_attempt_id"),
		ts("unlinked_at").olderNull(),
		ts("last_seen_at").olderNull(), text("ingress_ipv4").olderNull(), text("ingress_ipv6").olderNull())},
	{"node_snapshot_acks", cols(uuid("node_id"), text("content_hash"), num("snapshot_version"), num("replication_version"),
		uuid("revision_id"), num("leadership_generation"), ts("applied_at"), ts("received_at"))},
	{"sync_attempts", cols(uuid("id"), uuid("node_id"), enum("trigger", enumSyncTrigger), enum("status", enumSyncStatus),
		uuid("source_primary_id").null(), num("leadership_generation").null(), num("snapshot_version").null(),
		num("replication_version").null(), text("content_hash").null(), uuid("revision_id").null(),
		uuid("apply_job_id").null(), text("result_code").null(), ts("started_at").null(), ts("finished_at").null(),
		ts("created_at"), ts("updated_at"))},
	{"internal_ca_enrollment_state", cols(text("id"), ts("established_at"))},
}

// Tables returns a copy of the mapping in foreign-key-safe copy order.
func Tables() []TableSpec {
	out := make([]TableSpec, len(tables))
	copy(out, tables)
	return out
}

func tableSpec(name string) (TableSpec, error) {
	for _, t := range tables {
		if t.Name == name {
			return t, nil
		}
	}
	return TableSpec{}, fmt.Errorf("pgconvert: no mapping for table %q", name)
}
