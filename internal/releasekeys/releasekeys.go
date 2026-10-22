// Package releasekeys holds the public keys that release signatures are
// checked against, and the check itself. Each release's SHA256SUMS.txt is
// signed (Ed25519) in CI; SHA256SUMS.txt.sig holds the base64 signature.
package releasekeys

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
)

// Keys are the accepted signing keys. Releases are signed with the first;
// the second is a spare, so the first can be replaced (if it's ever lost or
// leaked) without existing installs losing their updates.
var Keys = []string{
	"9JzdnhnMFHPU2Buxd5LDtzbAPMRVTVW8ZQZ+tDDJgXw=", // secret RELEASE_SIGNING_KEY
	"bs4Cabad/CD+lNOC7GWeFo9KbAcNOL1ZnnhMC8OW1FE=", // secret RELEASE_SIGNING_KEY_RECOVERY
}

// Verify reports whether sig (base64) is a valid signature of msg by one of keys.
func Verify(msg []byte, sig string, keys []string) error {
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(s) != ed25519.SignatureSize {
		return errors.New("the release signature is malformed")
	}
	for _, k := range keys {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, msg, s) {
			return nil
		}
	}
	return errors.New("the release signature doesn't match this project's signing keys")
}
