package app

import (
	"strings"
	"testing"
)

func TestRenderLANSetupQRUsesHalfBlockMatrixWithQuietZone(t *testing.T) {
	rendered, width, err := renderLANSetupQR("http://192.168.1.42/__gohere/trust/token")
	if err != nil {
		t.Fatal(err)
	}
	if width < 20 || !strings.Contains(rendered, "▀") && !strings.Contains(rendered, "▄") && !strings.Contains(rendered, "█") {
		t.Fatalf("rendered QR width=%d:\n%s", width, rendered)
	}
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(lines) < 10 {
		t.Fatalf("rendered lines = %d", len(lines))
	}
	if strings.TrimSpace(strings.ReplaceAll(lines[0], "\x1b[30;47m", "")) != "\x1b[0m" {
		t.Fatalf("first quiet-zone line = %q", lines[0])
	}
}

func TestRenderLANSetupQRRejectsEmptyURL(t *testing.T) {
	if _, _, err := renderLANSetupQR(""); err == nil {
		t.Fatal("empty setup URL encoded")
	}
}
