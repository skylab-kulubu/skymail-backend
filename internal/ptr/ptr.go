// Package ptr compares optional values.
package ptr

// Equal reports whether a and b hold the same value, or are both nil: SQL's
// IS NOT DISTINCT FROM for an optional column.
func Equal[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
