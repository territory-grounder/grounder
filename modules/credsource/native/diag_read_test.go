package native

import (
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

// TG-687: the native hostdiag source IS TG's guarded read-only identity, so it must declare the capability the
// engine uses to prefer it for diagnostic reads over a higher-precedence admin binding.
// KILLING MUTATION: make ServesDiagRead return false — RED here.
func TestTheNativeHostdiagSourceDeclaresItselfTheReadOnlyIdentity(t *testing.T) {
	var src credential.CredentialSource = &Source{id: DefaultID}
	dr, ok := src.(credential.DiagReadSource)
	if !ok || !dr.ServesDiagRead() {
		t.Fatal("the native hostdiag source must declare credential.DiagReadSource (ServesDiagRead()=true)")
	}
}
