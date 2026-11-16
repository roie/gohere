package runner

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseTCPListeners(t *testing.T) {
	for _, test := range []struct {
		name, address, state, host string
		match, unknown             bool
	}{
		{"ipv4", "0100007F:0BB8", "0A", "127.0.0.1", true, false},
		{"wildcard", "00000000:0BB8", "0A", "127.0.0.1", true, false},
		{"ipv6", "00000000000000000000000001000000:0BB8", "0A", "::1", true, false},
		{"dual", "00000000000000000000000000000000:0BB8", "0A", "127.0.0.1", true, false},
		{"ipv4-not-ipv6", "00000000:0BB8", "0A", "::1", false, false},
		{"mapped", "0000000000000000FFFF00000100007F:0BB8", "0A", "127.0.0.1", true, false},
		{"wsl", "020A14AC:0BB8", "0A", "0.0.0.0", true, false},
		{"other-port", "0100007F:0BB9", "0A", "127.0.0.1", false, false},
		{"other-host", "0200007F:0BB8", "0A", "127.0.0.1", false, false},
		{"established", "0100007F:0BB8", "01", "127.0.0.1", false, false},
		{"invalid-address", "bad:0BB8", "0A", "127.0.0.1", false, true},
		{"invalid-port", "0100007F:unknown", "0A", "127.0.0.1", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := tcpTable(test.address, test.state, "42")
			owners, err := parseTCPListeners(output, net.ParseIP(test.host), 3000)
			if (len(owners) == 1 && owners[0] == "42") != test.match || (err != nil) != test.unknown {
				t.Fatalf("owners=%v error=%v", owners, err)
			}
		})
	}
}

func TestLinuxListenerRequiresAllSocketOwners(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			proc := t.TempDir()
			netDir := filepath.Join(proc, "net")
			fdDir := filepath.Join(proc, "123", "fd")
			if err := os.MkdirAll(netDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(fdDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("socket:[42]", filepath.Join(fdDir, "3")); err != nil {
				t.Fatal(err)
			}
			table := tcpTable("0100007F:0BB8", "0A", "42")
			if missing {
				_, row, _ := strings.Cut(tcpTable("00000000:0BB8", "0A", "43"), "\n")
				table += row
			}
			if err := os.WriteFile(filepath.Join(netDir, "tcp"), []byte(table), 0600); err != nil {
				t.Fatal(err)
			}
			owners, err := linuxListenerPIDs(t.Context(), proc, "127.0.0.1", 3000)
			if missing {
				if err == nil || len(owners) != 0 {
					t.Fatalf("unresolved socket accepted: owners=%v error=%v", owners, err)
				}
			} else if err != nil || !slices.Equal(owners, []int{123}) {
				t.Fatalf("owners=%v error=%v", owners, err)
			}
		})
	}
}

func TestParseProcessParent(t *testing.T) {
	for _, stat := range []string{"42 (node) S 24 0", "42 (name with ) spaces) S 24 0"} {
		parent, err := parseProcessParent(stat)
		if err != nil || parent != 24 {
			t.Fatalf("parent=%d error=%v", parent, err)
		}
	}
	for _, stat := range []string{"", "42 (node)", "42 (node) S bad", "42 (node) S -1"} {
		if _, err := parseProcessParent(stat); err == nil {
			t.Fatalf("invalid stat accepted: %q", stat)
		}
	}
}

func tcpTable(address, state, inode string) string {
	return fmt.Sprintf("sl local_address rem_address st tx_queue tr retr uid timeout inode\n0: %s 00000000:0000 %s 0:0 0:0 0 1000 0 %s\n", address, state, inode)
}

func TestReadinessWithoutPATHUtilities(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	result, err := Start(t.Context(), Config{
		Command:        []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", "print-port-sleep"},
		Env:            []string{"GOHERE_HELPER_PROCESS=1"},
		StartupTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stop()
	if result.Port <= 0 {
		t.Fatal("owned server was not detected")
	}
}
