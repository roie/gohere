package userpath

import "testing"

func TestHomeDirReturnsNonEmptyPath(t *testing.T) {
	if got := HomeDir(); got == "" {
		t.Fatal("HomeDir returned empty path")
	}
}
