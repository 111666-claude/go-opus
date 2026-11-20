package ratectl

import "fmt"

// errf is fmt.Errorf kept under one name so the sentinel-wrapping call sites read
// uniformly. All errors here wrap one of the package sentinels so errors.Is works.
func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
