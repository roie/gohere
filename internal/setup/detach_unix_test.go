//go:build !windows

package setup

import "testing"

func TestDetachedSysProcAttrStartsNewProcessGroup(t *testing.T) {
	attr := detachedSysProcAttr()
	if attr == nil || !attr.Setpgid {
		t.Fatalf("detachedSysProcAttr() = %#v, want Setpgid", attr)
	}
}
