//go:build !linux

package cli

import (
	"context"
	"fmt"
)

func serve(context.Context, Config) error {
	return fmt.Errorf("the daemon requires Linux; clients and config validation are portable")
}
