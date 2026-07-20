// rand.go wraps crypto/rand behind an indirection so Service tests can
// substitute a deterministic reader (avoids pulling in monkey-patching
// deps + keeps tests hermetic).

package giteasync

import "crypto/rand"

// randReader is the default entropy source for randomProvisioningPassword.
// Tests swap this for a deterministic reader; production never touches it.
var randReader = rand.Reader
