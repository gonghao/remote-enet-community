package netpriv

import (
	"strconv"
	"strings"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func authorizationScript(helper, socket string, uid int) string {
	command := "exec " + shellQuote(helper) + " --ipc " + shellQuote(socket) + " --uid " + strconv.Itoa(uid)
	// AppleScript and POSIX shell quoting are independent layers.
	return "do shell script \"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(command) + "\" with administrator privileges"
}
