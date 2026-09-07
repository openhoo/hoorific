package core

// CloseCredentialLease releases an optional credential lease closer. Credential
// leases deliberately keep closure out of the authorization interface so
// providers can use short-lived or reusable handles; owners must still call
// this helper when they acquire a lease. A nil lease or a lease without an
// optional closer is valid. If the closer returns an error, it is returned to
// the owner after the close attempt.
func CloseCredentialLease(lease CredentialLease) error {
	if lease == nil {
		return nil
	}
	switch closer := lease.(type) {
	case interface{ Close() error }:
		return closer.Close()
	case interface{ Close() }:
		closer.Close()
	}
	return nil
}
