// Command onoff controls a Bot whose address you already know, without
// scanning: it shows the Bot's status, turns it on, waits three seconds and
// turns it off. Commands after the first reuse the connection.
//
//	go run ./examples/onoff -addr AA:BB:CC:DD:EE:FF
//	go run ./examples/onoff -addr AA:BB:CC:DD:EE:FF -switch-mode   # put it in switch mode first
//
// Find your Bot's address with the command-line tool: switchbot scan.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/karoloslykos/switchbot"
)

func main() {
	addr := flag.String("addr", "", "address of the Bot, e.g. AA:BB:CC:DD:EE:FF (required)")
	switchMode := flag.Bool("switch-mode", false, "put the Bot in switch mode first; on/off only work in switch mode")
	flag.Parse()
	if *addr == "" {
		flag.Usage()
		log.Fatal("-addr is required; find your Bot's address with: switchbot scan")
	}
	if err := run(*addr, *switchMode); err != nil {
		log.Fatal(err)
	}
}

// run does the work, so that deferred cleanup runs before main exits.
func run(addr string, switchMode bool) (err error) {
	client, err := switchbot.NewClient()
	if err != nil {
		return err
	}
	// Close disconnects from the Bot.
	defer func() {
		if cerr := client.Close(); err == nil {
			err = cerr
		}
	}()

	bot, err := client.Bot(addr)
	if err != nil {
		return err
	}
	ctx := context.Background()

	// The first command connects to the Bot.
	start := time.Now()
	st, err := bot.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%s: battery %d%%, firmware %s, %s mode (connected in %s)\n",
		bot.Address(), st.Battery, st.Firmware, st.Mode, time.Since(start).Round(time.Millisecond))

	if st.Mode != switchbot.ModeSwitch {
		if !switchMode {
			return errors.New("the Bot is in press mode, where it can only press; run with -switch-mode to change it")
		}
		if err := bot.SetMode(ctx, switchbot.ModeSwitch); err != nil {
			return err
		}
		fmt.Println("Switched to switch mode.")
	}

	start = time.Now()
	if err := bot.TurnOn(ctx); err != nil {
		return err
	}
	fmt.Println("On  (took", time.Since(start).Round(time.Millisecond), ")")

	time.Sleep(3 * time.Second)

	start = time.Now()
	if err := bot.TurnOff(ctx); err != nil {
		return err
	}
	fmt.Println("Off (took", time.Since(start).Round(time.Millisecond), ")")
	return nil
}
