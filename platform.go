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
}
