package main

import (
	_ "embed"
	"log"

	"github.com/egoist/mygo"
)

//go:embed assets/tray.png
var trayIcon []byte

func main() {
	app := newTrayApp()
	mygo.App.SetName("mcpctl")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.WhenReady(app.start)
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
