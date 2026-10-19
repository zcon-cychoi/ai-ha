//go:build !linux

package rescue

import (
	"fmt"
	"runtime"
)

// isRAMBacked always errors outside Linux: the rescue environment is Linux.
func isRAMBacked(_ string) (bool, error) {
	return false, fmt.Errorf("the rescue agent only runs on linux, not %s", runtime.GOOS)
}
