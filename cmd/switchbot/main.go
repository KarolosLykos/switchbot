// Command switchbot controls SwitchBot Bots over Bluetooth LE from the
// command line. It is a small front end for the github.com/karoloslykos/switchbot library.
//
// Usage:
//
//	switchbot                          scan, pick a Bot, then type commands
//	switchbot scan                     list Bots in range
//	switchbot <address>                type commands for that Bot
//	switchbot <address> <command>      run one command
//
// Commands: on, off, press, down, up, status, mode press|switch,
// invert on|off, hold <seconds>.
//
// Flags (before the arguments):
//
//	-t 10s        scan duration
//	-p password   password set on the Bots in the SwitchBot app
//	-v            log every step, and every Bluetooth device seen
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/karoloslykos/switchbot"
	"tinygo.org/x/bluetooth"
)

const commandHelp = `Commands:
  on, off            turn on / off (switch mode)
  press              press the button (press mode)
  down, up           move the arm down / up and leave it
  status             battery, firmware and settings
  mode press|switch  change mode
  invert on|off      invert the arm's direction
  hold <seconds>     how long a press holds the button down`

func main() {
	scanTime := flag.Duration("t", 10*time.Second, "scan duration")
	password := flag.String("p", "", "password set on the Bots in the SwitchBot app")
	verbose := flag.Bool("v", false, "log every step, and every Bluetooth device seen")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: switchbot [-t 10s] [-p password] [-v] [scan | <address> [command]]")
		fmt.Fprintln(os.Stderr, commandHelp)
	}
	flag.Parse()

	var opts []switchbot.Option
	if *password != "" {
		opts = append(opts, switchbot.WithPassword(*password))
	}
	if *verbose {
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		opts = append(opts, switchbot.WithLogger(logger), switchbot.WithScanHook(printOnce()))
	}
	client, err := switchbot.NewClient(opts...)
	if err != nil {
		fail(err)
	}

	// Ctrl+C disconnects cleanly and quits, even while waiting for input.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		<-interrupt
		_ = client.Close()
		os.Exit(130)
	}()

	err = run(client, *scanTime, flag.Args())
	_ = client.Close()
	if err != nil {
		fail(err)
	}
}

func run(client *switchbot.Client, scanTime time.Duration, args []string) error {
	ctx := context.Background()

	switch {
	case len(args) == 0:
		bots, err := discover(ctx, client, scanTime)
		if err != nil {
			return err
		}
		bot, err := choose(bots)
		if err != nil {
			return err
		}
		return interactive(ctx, bot)
	case args[0] == "scan":
		bots, err := discover(ctx, client, scanTime)
		for _, b := range bots {
			fmt.Println(b)
		}
		return err
	}

	bot, err := client.Bot(args[0])
	if err != nil {
		return err
	}
	if len(args) == 1 {
		return interactive(ctx, bot)
	}
	return command(ctx, bot, args[1:])
}

func discover(ctx context.Context, client *switchbot.Client, scanTime time.Duration) ([]*switchbot.Bot, error) {
	fmt.Fprintf(os.Stderr, "Scanning for %s...\n", scanTime)
	bots, err := client.DiscoverBots(ctx, scanTime)
	if err == nil && len(bots) == 0 {
		err = errors.New("no Bots found; close the SwitchBot phone app, move closer, " +
			"or run with -v to see every Bluetooth device")
	}
	return bots, err
}

func choose(bots []*switchbot.Bot) (*switchbot.Bot, error) {
	for i, b := range bots {
		fmt.Printf("  [%d] %s\n", i+1, b)
	}
	if len(bots) == 1 {
		return bots[0], nil
	}
	fmt.Print("Choose a Bot: ")
	in := bufio.NewScanner(os.Stdin)
	if !in.Scan() {
		return nil, errors.New("no Bot chosen")
	}
	n, err := strconv.Atoi(strings.TrimSpace(in.Text()))
	if err != nil || n < 1 || n > len(bots) {
		return nil, errors.New("invalid choice")
	}
	return bots[n-1], nil
}

func interactive(ctx context.Context, bot *switchbot.Bot) error {
	fmt.Printf("\nControlling %s\n%s\n  q                  quit\n", bot.Address(), commandHelp)
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !in.Scan() {
			return nil
		}
		args := strings.Fields(strings.ToLower(in.Text()))
		switch {
		case len(args) == 0:
		case args[0] == "q" || args[0] == "quit" || args[0] == "exit":
			return nil
		default:
			if err := command(ctx, bot, args); err != nil {
				fmt.Println("error:", err)
			}
		}
	}
}

// command runs one command on a Bot and prints the result.
func command(ctx context.Context, bot *switchbot.Bot, args []string) error {
	arg := ""
	if len(args) > 1 {
		arg = args[1]
	}
	var err error
	switch args[0] {
	case "on":
		err = bot.TurnOn(ctx)
	case "off":
		err = bot.TurnOff(ctx)
	case "press", "p":
		err = bot.Press(ctx)
	case "down":
		err = bot.ArmDown(ctx)
	case "up":
		err = bot.ArmUp(ctx)
	case "status", "s":
		st, err := bot.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("battery %d%%  firmware %s  mode %s  inverted %v  hold %s  timers %d\n",
			st.Battery, st.Firmware, st.Mode, st.Inverted, st.HoldTime, st.Timers)
		return nil
	case "mode":
		switch arg {
		case "press":
			err = bot.SetMode(ctx, switchbot.ModePress)
		case "switch":
			err = bot.SetMode(ctx, switchbot.ModeSwitch)
		default:
			return errors.New("usage: mode press|switch")
		}
	case "invert":
		switch arg {
		case "on":
			err = bot.SetInverted(ctx, true)
		case "off":
			err = bot.SetInverted(ctx, false)
		default:
			return errors.New("usage: invert on|off")
		}
	case "hold":
		secs, convErr := strconv.Atoi(arg)
		if convErr != nil {
			return errors.New("usage: hold <seconds>")
		}
		err = bot.SetHoldTime(ctx, time.Duration(secs)*time.Second)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], commandHelp)
	}
	if errors.Is(err, switchbot.ErrUnsupported) {
		return fmt.Errorf("%w: use on/off in switch mode and press in press mode (change with 'mode')", err)
	}
	if err != nil {
		return err
	}
	fmt.Println("OK")
	return nil
}

// printOnce returns a scan hook that prints each distinct advertisement once.
func printOnce() func(bluetooth.ScanResult) {
	var (
		mu      sync.Mutex
		printed = map[string]bool{}
	)
	return func(r bluetooth.ScanResult) {
		line := describe(r)
		mu.Lock()
		defer mu.Unlock()
		if !printed[line] {
			printed[line] = true
			fmt.Fprintln(os.Stderr, "  seen:", line)
		}
	}
}

// describe prints everything useful in an advertisement.
func describe(r bluetooth.ScanResult) string {
	s := fmt.Sprintf("%-18s  RSSI %4d  name %q", r.Address.String(), r.RSSI, r.LocalName())
	for _, md := range r.ManufacturerData() {
		s += fmt.Sprintf("  mfr 0x%04X=% X", md.CompanyID, md.Data)
	}
	for _, u := range r.ServiceUUIDs() {
		s += "  svc " + u.String()
	}
	for _, sd := range r.ServiceData() {
		s += fmt.Sprintf("  svcdata %s=% X", sd.UUID.String(), sd.Data)
	}
	return s
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
