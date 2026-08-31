package main

type Platform interface {
	Init()
	Run()
	SetupTray(rgba []byte, w, h int)
	UpdateMenu(menuJSON string)
	OpenSettings(html string)
	EvalSettingsJS(js string)
	DispatchToMain(fn func())
	OpenURL(url string)

	// Notify raises a user notification. It is best-effort by design and
	// reports nothing back: macOS drops it when the user has denied
	// notifications, when Focus is on, or when the process has no bundle
	// identifier (a bare ./relay, or go test), and there is no way to force
	// one. Nothing may depend on delivery — the tray's own "Pending
	// enrolment requests: N" line is the guaranteed surface (ADR-019 spec
	// §9.4).
	Notify(title, body string)
}
