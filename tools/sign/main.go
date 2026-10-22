// Command sign signs a release's checksum list for the updater to verify:
//
//	RELEASE_SIGNING_KEY=... go run ./tools/sign dist/SHA256SUMS.txt   # writes dist/SHA256SUMS.txt.sig
//
// The private key (a base64 Ed25519 seed) comes from the environment so it is
// never written to disk; the release workflow passes the repository secret.
// The new signature is checked against the keys built into the program, so a
// wrong key fails the release instead of publishing a signature that updates
// would refuse.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"wiser-dashboard/internal/releasekeys"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: RELEASE_SIGNING_KEY=... go run ./tools/sign FILE")
		os.Exit(2)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		fail("RELEASE_SIGNING_KEY is missing or isn't a base64 Ed25519 key")
	}
	msg, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err.Error())
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), msg))
	if err := releasekeys.Verify(msg, sig, releasekeys.Keys); err != nil {
		fail("this key isn't one of the program's release keys (internal/releasekeys): " + err.Error())
	}
	if err := os.WriteFile(os.Args[1]+".sig", []byte(sig+"\n"), 0o644); err != nil {
		fail(err.Error())
	}
	fmt.Printf("signed %s -> %s.sig\n", os.Args[1], os.Args[1])
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "sign:", msg)
	os.Exit(1)
}
