package store_test

import (
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
)

func TestPagingEffectiveLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
		want int
	}{
		{"unset", 0, store.DefaultLimit},
		{"negative", -1, store.DefaultLimit},
		{"one", 1, 1},
		{"below the maximum", store.MaxLimit - 1, store.MaxLimit - 1},
		{"at the maximum", store.MaxLimit, store.MaxLimit},
		// A page size is client input, so an implausible one is clamped rather
		// than honoured.
		{"above the maximum", store.MaxLimit + 1, store.MaxLimit},
		{"absurd", 1 << 30, store.MaxLimit},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := (store.Paging{Limit: tc.in}).EffectiveLimit(); got != tc.want {
				t.Errorf("Paging{Limit: %d}.EffectiveLimit() = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestTokenActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		token store.Token
		want  bool
	}{
		{"no expiry, not revoked", store.Token{}, true},
		{"expires later", store.Token{ExpiresAt: now.Add(time.Hour)}, true},
		{"expires now", store.Token{ExpiresAt: now}, false},
		{"expired", store.Token{ExpiresAt: now.Add(-time.Second)}, false},
		{"revoked", store.Token{RevokedAt: now.Add(-time.Hour)}, false},
		// Revocation is immediate and beats an expiry still in the future.
		{"revoked but not yet expired", store.Token{
			RevokedAt: now.Add(-time.Hour),
			ExpiresAt: now.Add(time.Hour),
		}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.token.Active(now); got != tc.want {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}
