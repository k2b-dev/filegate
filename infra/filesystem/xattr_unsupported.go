//go:build !linux

package filesystem

import (
	"fmt"

	"github.com/k2b-dev/filegate/v3/domain"
)

func setID(_ string, _ domain.FileID) error {
	return fmt.Errorf("xattr only supported on linux builds")
}

func setIDIfAbsent(_ string, _ domain.FileID) (domain.FileID, bool, error) {
	return domain.FileID{}, false, fmt.Errorf("xattr only supported on linux builds")
}

func getID(_ string) (domain.FileID, error) {
	return domain.FileID{}, fmt.Errorf("xattr only supported on linux builds")
}
