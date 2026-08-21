package sqlite_test

import (
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/storetest"
)

// The SQLite backend has to pass the same suite every backend passes. When
// Postgres arrives it adds a file like this one and inherits every case, so
// "both behave the same behind the interface" is checked rather than claimed.
func TestConformance(t *testing.T) {
	t.Parallel()

	storetest.Run(t, func(t *testing.T) store.Store {
		return open(t)
	})
}
