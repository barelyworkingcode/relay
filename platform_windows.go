//go:build windows

package main

// WindowsPlatform is a stub for future Windows support.
type WindowsPlatform struct{}

func NewPlatform() Platform { return &WindowsPlatform{} }

func (p *WindowsPlatform) Init()                         { panic("not implemented") }
func (p *WindowsPlatform) Run()                          { panic("not implemented") }
func (p *WindowsPlatform) SetupTray(rgba []byte, w, h int) { panic("not implemented") }
func (p *WindowsPlatform) UpdateMenu(menuJSON string)    { panic("not implemented") }
func (p *WindowsPlatform) OpenSettings(html string)      { panic("not implemented") }
func (p *WindowsPlatform) CloseSettings()                { panic("not implemented") }
func (p *WindowsPlatform) EvalSettingsJS(js string)      { panic("not implemented") }
func (p *WindowsPlatform) CopyToClipboard(text string)   { panic("not implemented") }
func (p *WindowsPlatform) DispatchToMain(fn func())      { panic("not implemented") }
func (p *WindowsPlatform) OpenURL(url string)            { panic("not implemented") }
