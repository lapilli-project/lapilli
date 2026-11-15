package ledger

import "strings"

// TeamNameOK reports whether PutTeam takes a name for a team's: letters,
// digits, spaces and . _ -, up to 64, as it is, with no space around it.
func TeamNameOK(name string) bool {
	return name == strings.TrimSpace(name) && teamName.MatchString(name)
}
