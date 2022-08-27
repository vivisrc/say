package main

import (
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/joho/godotenv"
	"github.com/vivisrc/say"
	"github.com/vivisrc/say/mod/voice"
)

var (
	registerCommands = flag.Bool("register", false, "Register commands on startup")
)

func init() {
	flag.Parse()
}

func main() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalln("Error loading env file:", err)
	}

	bot, err := say.New()
	if err != nil {
		log.Fatalln("Failed to create bot:", err)
	}

	if err := voice.Install(bot); err != nil {
		log.Fatalln("Failed to install voice module:", err)
	}

	if *registerCommands {
		bot.RegisterCommands()
	}

	if err := bot.Open(); err != nil {
		log.Fatalln("Failed to start bot:", err)
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt

	if err := bot.Close(); err != nil {
		log.Fatalln("Failed to stop bot:", err)
	}
}
