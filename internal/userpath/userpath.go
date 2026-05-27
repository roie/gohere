package userpath

import "os"

func HomeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}
