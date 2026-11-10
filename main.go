package main

import (
	"log"
	"os"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	deskui "piui/internal/ui"
)

func main() {
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}

	app, err := deskui.NewDeskApp(workDir)
	if err != nil {
		log.Fatalf("failed to initialize PiUI DeskApp: %v", err)
	}

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
