package enrollment

import (
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestVerifyIdentityProofRequiresExactExpectedGeneration(t *testing.T) {
	f := newProofFixture(t)
	for _, generation := range []uint64{uint64(f.id.LeadershipGeneration - 1), uint64(f.id.LeadershipGeneration + 1), 0} {
		opts := f.options(f.now)
		opts.ExpectedLeadershipGeneration = domain.LeadershipGeneration(generation)
		if err := VerifyIdentityProof(f.proof, opts); err == nil {
			t.Fatalf("generation %d was accepted; expected exact generation %d", generation, f.id.LeadershipGeneration)
		}
	}
}
