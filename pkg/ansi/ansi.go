package ansi

import "regexp"

// stripRe matches ANSI escape sequences (compiled once at startup).
var stripRe = regexp.MustCompile(
	`[\x1b\x9b][\[\]()#;?]*(?:(?:[a-zA-Z\d]*(?:;[a-zA-Z\d]*)*)?\x07|(?:\d{1,4}(?:;\d{0,4})*)?[\dA-PRZcf-ntqry=><~])`,
)

// Strip removes all ANSI escape sequences from s.
func Strip(s string) string {
	return stripRe.ReplaceAllString(s, "")
}
