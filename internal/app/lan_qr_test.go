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
	if width < 25 || !strings.ContainsAny(rendered, "▀▄█") {
		t.Fatalf("rendered QR width=%d:\n%s", width, rendered)
	}
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(lines) < 10 {
		t.Fatalf("rendered lines = %d", len(lines))
	}
	if strings.TrimSpace(lines[0]) != "" {
		t.Fatalf("first quiet-zone line = %q", lines[0])
	}
}

func TestRenderLANSetupQRFitsCompactBootstrapURL(t *testing.T) {
	rendered, width, err := renderLANSetupQR("http://192.168.1.42/g/AbCdEfGhIjKlMnOpQrStUv")
	if err != nil {
		t.Fatal(err)
	}
	if width > 45 {
		t.Fatalf("terminal QR width = %d, want at most 45", width)
	}
	if strings.Contains(rendered, "\x1b[30;47m") {
		t.Fatal("terminal QR forces a white background")
	}
}

func TestRenderLANSetupQRRejectsEmptyURL(t *testing.T) {
	if _, _, err := renderLANSetupQR(""); err == nil {
		t.Fatal("empty setup URL encoded")
	}
}
