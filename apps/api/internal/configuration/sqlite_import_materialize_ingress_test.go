package configuration

import (
	"context"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	persistedNodeIPv4 = "198.51.100.200"
	persistedNodeIPv6 = "2001:db8::200"
)

// A sync must never replace the node's persisted advertised ingress, whatever
// the snapshot carries (a primary ingress, a different nodeLocal ingress, or
// none), while the resolver pool and forwarding rules are still replaced.
func TestSQLiteImportKeepsPersistedNodeIngress(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*replicationsnapshot.Envelope)
	}{
		{"nodeLocal ingress differs from the persisted one", nil},
		{"nodeLocal ingress is empty", func(env *replicationsnapshot.Envelope) { env.NodeLocal.Ingress = domain.Ingress{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, store := newSQLitePhase2Fixture(t)
			settingsExec(t, f.db, `insert into installation_settings (id,forwarding_rules,created_at,updated_at) values (?, '[]', '0', '0')
				on conflict(id) do nothing`, installationID)
			settingsExec(t, f.db, `update installation_settings set ingress_ipv4=?, ingress_ipv6=? where id=?`,
				persistedNodeIPv4, persistedNodeIPv6, installationID)
			cfg := materializeConfig() // carries the primary's ingress 203.0.113.1 / 2001:db8::1
			body, _ := materializeEnvelopeBody(t, f.primaryID, cfg, tc.mutate)

			if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body); err != nil {
				t.Fatal(err)
			}

			got := readNodeConfiguration(t, f).Settings
			if want := (domain.Ingress{IPv4: persistedNodeIPv4, IPv6: persistedNodeIPv6}); got.Ingress != want {
				t.Fatalf("settings ingress = %+v, want the node's persisted %+v", got.Ingress, want)
			}
			if got.DefaultPool == nil || got.DefaultPool.ID != cfg.Settings.DefaultPool.ID {
				t.Fatalf("default pool = %+v, want the primary's replaced pool", got.DefaultPool)
			}
		})
	}
}

// A node that never persisted an ingress does not inherit the primary's one
// either: the column stays empty until the node initializes its own.
func TestSQLiteImportDoesNotAdoptPrimaryIngressWhenNodeHasNone(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	body, _ := materializeEnvelopeBody(t, f.primaryID, materializeConfig(), func(env *replicationsnapshot.Envelope) {
		env.NodeLocal.Ingress = domain.Ingress{}
	})

	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body); err != nil {
		t.Fatal(err)
	}
	if got := readNodeConfiguration(t, f).Settings.Ingress; got != (domain.Ingress{}) {
		t.Fatalf("settings ingress = %+v, want empty (the primary's must not be adopted)", got)
	}
}
