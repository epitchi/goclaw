package http

import "regexp"

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// isValidSlug checks whether s matches the slug format: lowercase alphanumeric + hyphens,
// cannot start or end with a hyphen.
func isValidSlug(s string) bool {
	return slugRe.MatchString(s)
}
