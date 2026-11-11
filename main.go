package main

import (
	"log"
	"os"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"piui/internal/diag"
	"piui/internal/store"
	"piui/internal/sysenv"
	deskui "piui/internal/ui"
)

func main() {
	_ = diag.Init()
	defer func() {
		diag.CapturePanic("main", nil)
		_ = diag.Close()
	}()

	// Hydrate PATH & developer tool environment when launched as a GUI .app bundle.
	sysenv.HydrateLoginEnvironment()

	// Store API keys in OS Keychain / Credential Manager instead of plaintext JSON.
	if os.Getenv("PIUI_DISABLE_KEYCHAIN") != "1" {
		store.SetSecretStore(store.NewSystemKeychainStore())
	}

	workDir, explicitDir := sysenv.ResolveStartupWorkDir(os.Args[1:])

	app, err := deskui.NewDeskAppWithOptions(store.LoadOptions{
		StartupDir:         workDir,
		ExplicitStartupDir: explicitDir,
	})
	if err != nil {
		log.Fatalf("failed to initialize PiUI DeskApp: %v", err)
	}
	defer func() {
		_ = app.Close()
	}()

	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:           "PiUI — Native Pi Agent Desk",
			Width:           1360,
			Height:          840,
			MinWidth:        920,
			MinHeight:       580,
			TitleBarStyle:   mygo.TitleBarHiddenInset,
			BackgroundColor: "light-dark(#FAFAFC, #0E0E12)",
			StateKey:        "piui-main-window",
			Content:         ui.View(app.View),
		})
		app.BindWindow(win)
	})

	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
