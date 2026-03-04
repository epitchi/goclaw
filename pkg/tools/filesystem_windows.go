//go:build windows

package tools

// hasMutableSymlinkParent returns false on Windows — symlink rebind attacks
// are not applicable in the same way as on Unix.
func hasMutableSymlinkParent(_ string) bool {
	return false
}

// checkHardlink is a no-op on Windows. Windows hardlink semantics differ from
// Unix and the nlink check via syscall.Stat_t is not available.
func checkHardlink(_ string) error {
	return nil
}
