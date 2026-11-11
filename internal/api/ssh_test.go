package api

import "testing"

// bugs-r11-opus 1: guest-chosen values that land in ssh files.
func TestSSHValuesAreSingleTokens(t *testing.T) {
	for _, u := range []string{"user", "ubuntu", "_svc", "a.b-c"} {
		if !ValidSSHUser(u) {
			t.Errorf("user %q refused", u)
		}
	}
	for _, u := range []string{"", "user\n  ProxyCommand id", "User user", "root;id", "-oProxyCommand=x", "Ab"} {
		if ValidSSHUser(u) {
			t.Errorf("user %q accepted", u)
		}
	}
	for _, k := range []string{"ssh-ed25519 AAAA", "bx_1 ssh-ed25519 AAAA", "ecdsa-sha2-nistp256 AAAA"} {
		if !ValidHostKey(k) {
			t.Errorf("key %q refused", k)
		}
	}
	for _, k := range []string{"ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 BBBB", "@cert-authority * ssh-ed25519 AAAA", "ssh-ed25519 !!", "ssh-ed25519"} {
		if ValidHostKey(k) {
			t.Errorf("key %q accepted", k)
		}
	}
}
