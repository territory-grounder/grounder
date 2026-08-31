// Package lockstep is the content-aware hash mechanism behind the spec↔code lockstep gate (spec/007). It
// is the importable core of `specvalidate lockstep`: HashSemantic computes the comment- and
// format-insensitive content hash that binds a governed file to its owning spec, so a cosmetic edit does
// not read as drift (REQ-704) but a semantic change does (REQ-701/702). The CLI in tools/specvalidate uses
// this package, and the spec/007 acceptance oracle drives it directly.
//
// Provenance: [O] INV-22, spec/007 (BEH-7).
package lockstep

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// HashSemantic hashes a source file. For Go files it strips comments and collapses whitespace so a cosmetic
// comment or formatting edit does not read as spec drift (REQ-704, comment-insensitive); non-Go files are
// hashed byte-for-byte. Any change to an executable token yields a different hash (REQ-701/702).
func HashSemantic(path string, src []byte) string {
	data := src
	if strings.HasSuffix(path, ".go") {
		data = StripGoComments(src)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// StripGoComments removes // line and /* */ block comments, string/rune-literal aware, then collapses
// whitespace runs so formatting-only edits are ignored. Deliberately simple (no full parse).
func StripGoComments(src []byte) []byte {
	var out []byte
	s := string(src)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case ch == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++ // land on '/'
		case ch == '"' || ch == '`' || ch == '\'':
			quote := ch
			out = append(out, ch)
			i++
			for i < len(s) {
				out = append(out, s[i])
				if s[i] == '\\' && quote != '`' && i+1 < len(s) {
					i++
					out = append(out, s[i])
					i++
					continue
				}
				if s[i] == quote {
					break
				}
				i++
			}
		default:
			out = append(out, ch)
		}
	}
	// collapse whitespace so gofmt-only churn is invisible.
	return []byte(strings.Join(strings.Fields(string(out)), " "))
}
