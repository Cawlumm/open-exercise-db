package store

// StoreEscapeLikePattern exposes escapeLikePattern for the api package tests.
func StoreEscapeLikePattern(s string) string { return escapeLikePattern(s) }
