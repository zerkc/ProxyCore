package enrollment

// NewServiceWithIdentity wires the owner token service to the live identity
// cache. PNE-1a remains usable with NewService; this constructor is the
// activation-integrated PNE-1b path.
func NewServiceWithIdentity(store Store, live IdentityActivation, opts Options) *Service {
	opts.Identity = live
	return NewService(store, opts)
}
