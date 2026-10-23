package main

import (
	"context"
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestNodeSyncIngressResolverPrefersPersistedThenEnv(t *testing.T) {
	env := domain.Ingress{IPv4: "192.0.2.1", IPv6: "2001:db8::1"}
	persisted := domain.Ingress{IPv4: "198.51.100.9", IPv6: "2001:db8::9"}
	cases := []struct {
		name     string
		settings func(context.Context) (domain.Settings, error)
		want     domain.Ingress
	}{
		{"persisted wins over env", func(context.Context) (domain.Settings, error) {
			return domain.Settings{Ingress: persisted}, nil
		}, persisted},
		{"persisted ipv4 only still wins", func(context.Context) (domain.Settings, error) {
			return domain.Settings{Ingress: domain.Ingress{IPv4: "198.51.100.9"}}, nil
		}, domain.Ingress{IPv4: "198.51.100.9"}},
		{"env is the fallback when nothing is persisted", func(context.Context) (domain.Settings, error) {
			return domain.Settings{}, nil
		}, env},
		{"env is the fallback when the settings read fails", func(context.Context) (domain.Settings, error) {
			return domain.Settings{}, errors.New("read failed")
		}, env},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolve := nodeSyncIngressResolver(tc.settings, env)
			if got := resolve(context.Background()); got != tc.want {
				t.Fatalf("ingress = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNodeSyncIngressResolverNilSettingsUsesEnv(t *testing.T) {
	env := domain.Ingress{IPv4: "192.0.2.1"}
	if got := nodeSyncIngressResolver(nil, env)(context.Background()); got != env {
		t.Fatalf("ingress = %+v, want %+v", got, env)
	}
}
