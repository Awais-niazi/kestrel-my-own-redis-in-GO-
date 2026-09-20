package engine

// The write-ordering assertion (SetWriteOrderingAssertions) is left off for
// this package's tests and turned on for the whole of command's and server's,
// where the dispatcher is what takes the lock.
//
// It is a check on the caller, and the caller it is meant to check is the
// command layer. The tests here call DB methods directly, so none of them
// hold the propagation lock unless they say so, and an always-on assertion
// would fire on every write in the package rather than on the mistake. The
// tests that are about the ordering take the lock themselves.
