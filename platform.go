package main

// Platform abstracts OS-specific UI operations (tray, settings window, etc.).
type Platform interface {
	Init()
	Run()
	SetupTray(rgba []byte, w, h int)
	UpdateMenu(menuJSON string)
	OpenSettings(html string)
	CloseSettings()
	EvalSettingsJS(js string)
	CopyToClipboard(text string)
	DispatchToMain(fn func())
	OpenURL(url string)
}
