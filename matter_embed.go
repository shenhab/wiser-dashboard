//go:build matter

package main

import _ "embed"

// The release pipeline compiles matter/ for the target platform into
// matterbin/wiser-matter before building with -tags matter.
//
//go:embed matterbin/wiser-matter
var embeddedMatter []byte

func init() { matterBinary = embeddedMatter }
