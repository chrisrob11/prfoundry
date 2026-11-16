// Package itemname holds the rule for what a role, pipeline or repo may be
// called. Repo names are folders in the prf home, and role names appear in
// phase=<name> status lines, so the rule keeps both safe.
package itemname

import "regexp"

var re = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Valid reports whether name may name a role, pipeline or repo.
func Valid(name string) bool {
	return re.MatchString(name)
}
