//go:build !unix

package auth

import "context"

// Lock is a no-op where flock is unavailable (only Linux and macOS are
// released).
func (s *Store) Lock(_ context.Context, alias string) (func(), error) {
	if err := ValidateAlias(alias); err != nil {
		return nil, err
	}
	return func() {}, nil
}
