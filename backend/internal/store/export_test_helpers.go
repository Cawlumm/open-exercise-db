package store

// This file exposes two unexported helpers so internal/api's tests can assert
// on them directly. They live in a normal file rather than a _test.go one
// because a test file is not importable from another package's tests, and the
// behaviour being pinned — LIKE escaping and the search-term bound — is what
// internal/api's handlers depend on being correct.

// StoreEscapeLikePattern exposes escapeLikePattern for the api package tests.
func StoreEscapeLikePattern(s string) string { return escapeLikePattern(s) }

// StoreSearchTerms exposes searchTerms for the api package tests.
func StoreSearchTerms(q string) []string { return searchTerms(q) }
