//go:build !windows

package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func hasMutableSymlinkParent(path string) bool {
	clean := filepath.Clean(path)
	components := strings.Split(clean, string(filepath.Separator))
	current := string(filepath.Separator)
	for _, comp := range components {
		if comp == "" {
			continue
		}
		current = filepath.Join(current, comp)
		info, err := os.Lstat(current)
		if err != nil {
			break
		}
		if info.Mode()&os.ModeSymlink != 0 {
			parentDir := filepath.Dir(current)
			if syscall.Access(parentDir, 0x2 /* W_OK */) == nil {
				return true
			}
		}
	}
	return false
}

func checkHardlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if info.IsDir() {
		return nil
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if stat.Nlink > 1 {
			slog.Warn("security.hardlink_rejected", "path", path, "nlink", stat.Nlink)
			return fmt.Errorf("access denied: hardlinked file not allowed")
		}
	}
	return nil
}
