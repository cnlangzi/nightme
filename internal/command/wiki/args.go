package wiki

// usageErr is the sentinel returned for any non-zero argv tail.
// Message matches Wiki.md §3.1 "usage: /wiki".
type usageError struct{}

func (usageError) Error() string { return "usage: /wiki" }

var usageErr usageError

// parseWikiArgs enforces Wiki.md §3: /wiki accepts no flags and
// no positional arguments. Even -a / --agent — accepted by the
// v0 stub-mode wiki — is rejected here. Agent selection is the
// exclusive responsibility of /use.
//
// argv is the full Args slice from SlashInput; element 0 is
// always the command name. A non-zero length means the user
// passed trailing arguments.
func parseWikiArgs(argv []string) error {
	if len(argv) <= 1 {
		return nil
	}
	return usageErr
}
