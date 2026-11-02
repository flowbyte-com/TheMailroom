package config

// EffectiveMaxListScanMessages returns the list and search scan budget
// an account with these limits actually enforces.
//
// The configured value is authoritative when it is positive. A
// non-positive value means the limit is unset, and the default applies.
// Every component that enforces or reports the budget must use this
// helper, so the number an operator reads and the number the server
// applies cannot drift apart.
//
// The value is the client's own work bound. It does not bound the work
// the server does to evaluate a search's criteria.
func (l LimitsConfig) EffectiveMaxListScanMessages() int {
	if l.MaxListScanMessages > 0 {
		return l.MaxListScanMessages
	}
	return DefaultMaxListScanMessages
}
