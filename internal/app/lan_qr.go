package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	go_qr "github.com/piglig/go-qr"
)

const lanQRQuietZone = 4

func renderLANSetupQR(setupURL string) (string, int, error) {
	if setupURL == "" {
		return "", 0, errors.New("setup URL is required")
	}
	code, err := go_qr.EncodeText(setupURL, go_qr.Medium)
	if err != nil {
		return "", 0, err
	}
	width := code.Size() + 2*lanQRQuietZone
	var output strings.Builder
	for y := -lanQRQuietZone; y < code.Size()+lanQRQuietZone; y += 2 {
		output.WriteString("\x1b[30;47m")
		for x := -lanQRQuietZone; x < code.Size()+lanQRQuietZone; x++ {
			top := code.Module(x, y)
			bottom := code.Module(x, y+1)
			switch {
			case top && bottom:
				output.WriteRune('█')
			case top:
				output.WriteRune('▀')
			case bottom:
				output.WriteRune('▄')
			default:
				output.WriteByte(' ')
			}
		}
		output.WriteString("\x1b[0m\n")
	}
	return output.String(), width, nil
}

func maybePrintLANSetupQR(output io.Writer, setupURL string) {
	file, ok := output.(*os.File)
	if !ok {
		return
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return
	}
	rendered, width, err := renderLANSetupQR(setupURL)
	if err != nil || width > terminalColumns() {
		return
	}
	_, _ = fmt.Fprint(output, rendered)
}

func terminalColumns() int {
	if value, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && value > 0 {
		return value
	}
	return 80
}
