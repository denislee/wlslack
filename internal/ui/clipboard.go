package ui

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"gioui.org/io/clipboard"
	"gioui.org/layout"
)

// writeClipboardText puts text on the system clipboard.
//
// On Wayland we shell out to wl-copy rather than using Gio's clipboard.WriteCmd.
// Gio serves the wl_data_source from our own event loop: the compositor's
// `send` event only fires when our main goroutine pumps wl_display_dispatch,
// so a receiving GUI app blocks on its pipe read whenever we're idle or busy.
// wl-copy forks a small daemon that serves the clipboard independently, which
// the receiver can always read from without waiting on us.
func writeClipboardText(gtx layout.Context, text string) {
	if text == "" {
		return
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		cmd := exec.Command("wl-copy", "--type", "text/plain;charset=utf-8")
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Start(); err == nil {
			go func() { _ = cmd.Wait() }()
			return
		} else {
			slog.Warn("wl-copy failed, falling back to gio clipboard", "error", err)
		}
	}
	gtx.Execute(clipboard.WriteCmd{
		Type: "application/text",
		Data: io.NopCloser(strings.NewReader(text)),
	})
}
