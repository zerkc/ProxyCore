package enrollment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

type stubStore struct{}

func (*stubStore) CreateEnrollmentToken(context.Context, string, string, string, string, string, time.Time, time.Time) (domain.TopologyRole, error) {
	return domain.TopologyRolePrimary, nil
}
func (*stubStore) CheckEnrollmentToken(context.Context, string, string, time.Time, bool) error {
	return nil
}
func (*stubStore) RevokeEnrollmentToken(context.Context, string, string, time.Time) error { return nil }

func owner() auth.User { return auth.User{ID: uuid.NewString(), Role: auth.RoleOwner, Active: true} }

func TestCreateTokenRequiresActiveOwner(t *testing.T) {
	for _, user := range []auth.User{
		{ID: "operator", Role: auth.RoleOperator, Active: true},
		{ID: "inactive", Role: auth.RoleOwner},
	} {
		_, err := NewService(nil, Options{}).CreateToken(context.Background(), user)
		if !errors.Is(err, ErrOwnerRequired) {
			t.Fatalf("CreateToken(%+v) error = %v", user, err)
		}
	}
}

func TestCreateTokenFormatAndEntropy(t *testing.T) {
	created, err := NewService(&stubStore{}, Options{TTL: 10 * time.Minute}).CreateToken(context.Background(), owner())
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	parts := strings.SplitN(created.Token, "_", 3)
	if len(parts) != 3 {
		t.Fatalf("token format has %d sections, want 3", len(parts))
	}
	if parts[0] != tokenPrefix || len(parts[1]) != tokenSelectorLen*2 || len(parts[2]) != 43 {
		t.Fatalf("token format lengths invalid: prefix=%q selectorLen=%d secretLen=%d", parts[0], len(parts[1]), len(parts[2]))
	}
}
