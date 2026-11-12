package api

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// The guest answers the SSH key call with its login user and host key; the
// CLI writes both into ssh configuration and known_hosts. A hostile guest
// must not smuggle lines into those files (bugs-r11-opus 1).

var sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)

// ValidSSHUser reports a plain Unix login name.
func ValidSSHUser(u string) bool { return sshUserRe.MatchString(u) }

var sshKeyTypeRe = regexp.MustCompile(`^(ssh-|ecdsa-|sk-)[A-Za-z0-9@.-]+$`)

// ValidHostKey reports one "type base64" host key (an optional leading host
// field, as ssh-keyscan prints, is allowed) on a single line.
func ValidHostKey(k string) bool {
	k = strings.TrimSpace(k)
	if k == "" || strings.ContainsAny(k, "\r\n\x00") {
		return false
	}
	f := strings.Fields(k)
	if len(f) == 3 && !sshKeyTypeRe.MatchString(f[0]) {
		f = f[1:]
	}
	if len(f) != 2 || !sshKeyTypeRe.MatchString(f[0]) {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(f[1])
	return err == nil
}
