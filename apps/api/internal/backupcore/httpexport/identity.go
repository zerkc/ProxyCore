package httpexport

import "github.com/zerkc/ProxyCore/apps/api/internal/identity"

// IdentityAdapter exposes the loaded identity.Service through the manifest
// identity port without coupling the exporter to the service's concrete type.
type IdentityAdapter struct {
	Service *identity.Service
}

var _ IdentitySource = IdentityAdapter{}

// NewIdentitySource constructs an IdentitySource for a running identity service.
func NewIdentitySource(service *identity.Service) IdentitySource {
	return IdentityAdapter{Service: service}
}

func (a IdentityAdapter) current() identity.Identity {
	if a.Service == nil {
		return identity.Identity{}
	}
	return a.Service.Current()
}

func (a IdentityAdapter) InstallationIDString() string { return a.current().InstallationID.String() }
func (a IdentityAdapter) NodeIDString() string         { return a.current().NodeID.String() }
func (a IdentityAdapter) RoleString() string           { return a.current().Role.String() }
