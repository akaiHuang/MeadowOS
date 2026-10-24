package main

import "github.com/wailsapp/wails/v3/pkg/application"

const desktopComposerSubmitEvent = "meadow:composer-submit"

func emitDesktopComposerSubmit() {
	if app := application.Get(); app != nil && app.Event != nil {
		app.Event.Emit(desktopComposerSubmitEvent)
	}
}

// desktopComposerKeyBindings gives macOS accessibility and keyboard-only
// clients a native path into the same semantic form submission used by the
// visible send button. The frontend's send-intent guard collapses a native
// shortcut and a WebKit key event if both are delivered for one gesture.
func desktopComposerKeyBindings() map[string]func(application.Window) {
	return map[string]func(application.Window){
		"CmdOrCtrl+Return": func(_ application.Window) {
			emitDesktopComposerSubmit()
		},
	}
}

func addDesktopComposerMenu(menu *application.Menu, _ application.Window) {
	messageMenu := menu.AddSubmenu("訊息")
	messageMenu.Add("送出訊息").
		SetAccelerator("CmdOrCtrl+Return").
		OnClick(func(_ *application.Context) {
			emitDesktopComposerSubmit()
		})
}
