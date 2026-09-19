package httpadapter

import (
	"context"

	api "github.com/k2b-dev/filegate/v6/api/v1"
	"github.com/k2b-dev/filegate/v6/domain"
)

// transferDestination resolves only the destination view. Source reads always
// retain the actor bound by the authenticated request header.
func transferDestination(ctx context.Context, source, destination *domain.Root, target *api.ExecutionContext) (*domain.Root, func(), error) {
	identity := source.Execution()
	if target != nil {
		switch target.Mode {
		case "service":
			if target.Identity != nil {
				return nil, nil, domain.ErrInvalid
			}
			identity = nil
		case "unix":
			if target.Identity == nil {
				return nil, nil, domain.ErrInvalid
			}
			var err error
			identity, err = domain.NormalizeExecution(target.Identity)
			if err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, domain.ErrInvalid
		}
	}
	if destination.Config.Name == source.Config.Name {
		if !sameExecution(source.Execution(), identity) {
			return nil, nil, domain.ErrInvalid
		}
		return source, func() {}, nil
	}
	return destination.WithExecution(ctx, identity)
}
