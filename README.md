# switchbot

[![CI](https://github.com/karoloslykos/switchbot/actions/workflows/ci.yml/badge.svg)](https://github.com/karoloslykos/switchbot/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/karoloslykos/switchbot/graph/badge.svg)](https://codecov.io/gh/karoloslykos/switchbot)
[![Go Reference](https://pkg.go.dev/badge/github.com/karoloslykos/switchbot.svg)](https://pkg.go.dev/github.com/karoloslykos/switchbot)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A Go library for controlling [SwitchBot Bots](https://www.switch-bot.com/products/switchbot-bot) **directly over Bluetooth**, with **no internet, no cloud and no SwitchBot Hub**.

```
  your Go program  ──── Bluetooth LE ────►  SwitchBot Bot
        (no internet, no SwitchBot account, no Hub, no API token)
```

SwitchBot's official apps and APIs normally go through the SwitchBot cloud, which needs an account, an internet connection and often a Hub. This library skips all of that. It talks to the Bot the same way the phone app does when you stand next to it: over Bluetooth Low Energy, from any computer within range (about 10 m indoors).

- **Offline**: works on an air-gapped network, in a cabin, or when SwitchBot's servers are down.
- **Private**: nothing leaves your machine. No account, no tokens, no telemetry.
- **Fast**: the connection is kept open, so each command takes about 20–80 ms after the first.
- **Cross-platform**: Windows 10/11, Linux (BlueZ) and macOS, via [tinygo.org/x/bluetooth](https://github.com/tinygo-org/bluetooth).

## Install

```
go get github.com/karoloslykos/switchbot
```

Requires Go 1.25 or newer and a computer with Bluetooth LE.

## Quick start

```go
package main

import (
	"context"
	"log"

	"github.com/karoloslykos/switchbot"
)

func main() {
	client, err := switchbot.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	bot, err := client.Bot("AA:BB:CC:DD:EE:FF") // your Bot's address
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	if err := bot.TurnOn(ctx); err != nil {
		log.Fatal(err)
	}
}
```

Don't know the address? Scan for it:

```go
bots, err := client.DiscoverBots(ctx, 10*time.Second) // only Bots are returned
for _, b := range bots {
	fmt.Println(b.Address())
}
```

The command-line tool below can also do it: `switchbot scan`.

## Usage

### Commands

```go
bot.TurnOn(ctx)       // switch mode
bot.TurnOff(ctx)      // switch mode
bot.Press(ctx)        // press mode
bot.ArmDown(ctx)      // push the arm down and keep it there
bot.ArmUp(ctx)        // pull it back up

st, err := bot.Status(ctx) // st.Battery, st.Firmware, st.Mode, st.Inverted, st.HoldTime, st.Timers

bot.SetMode(ctx, switchbot.ModeSwitch) // or switchbot.ModePress
bot.SetInverted(ctx, true)             // flip the arm direction
bot.SetHoldTime(ctx, 2*time.Second)    // how long a press holds the button down
```

A Bot works in one of two modes. In **press mode** it presses the button and lets go, so use `Press`. In **switch mode** it remembers on and off, for rocker switches, so use `TurnOn` and `TurnOff`. Sending a command the current mode doesn't support returns `ErrUnsupported`.

### Connections

You don't manage connections yourself:

- The first command connects, and checks that the device really is a Bot (`ErrNotABot` otherwise).
- Later commands reuse the connection.
- If the connection drops, the next command reconnects by itself.
- After a minute without commands the Bot is disconnected, so it doesn't drain its battery or block the SwitchBot app. Change this with `WithIdleTimeout`.
- `bot.Close()` or `client.Close()` disconnects right away.

### Options

```go
client, err := switchbot.NewClient(
	switchbot.WithLogger(slog.Default()),        // log every step (silent by default)
	switchbot.WithPassword("secret"),            // if your Bots have a password in the SwitchBot app
	switchbot.WithIdleTimeout(5*time.Minute),    // stay connected longer (0 = until Close)
	switchbot.WithRetries(3),                    // retry failed connections or a busy Bot (default 2)
)
```

A command is only retried when the Bot can't have carried it out, so a press never happens twice by accident.

### Cancellation

Every method takes a `context.Context`, which is used only for cancelling. If it's cancelled, the method stops and returns `ctx.Err()`. The scan length is a separate argument of `DiscoverBots`.

### Errors

When a Bot rejects a command, you get a `*switchbot.StatusError` holding its status code. Check for the common cases with `errors.Is`:

```go
switch err := bot.TurnOn(ctx); {
case errors.Is(err, switchbot.ErrUnsupported):      // wrong mode for this command
case errors.Is(err, switchbot.ErrBusy):             // still busy after retries
case errors.Is(err, switchbot.ErrLowBattery):
case errors.Is(err, switchbot.ErrPasswordRequired): // use WithPassword
case errors.Is(err, switchbot.ErrWrongPassword):
case errors.Is(err, switchbot.ErrNotABot):          // that address isn't a Bot
case errors.Is(err, switchbot.ErrNotFound):         // out of range or turned off
}
```

`ErrNoResponse` and `ErrConnectionLost` mean the Bot didn't answer in time. The command may or may not have run.

See the [package documentation](https://pkg.go.dev/github.com/karoloslykos/switchbot) for the full API.

## Command-line tool

```
go install github.com/karoloslykos/switchbot/cmd/switchbot@latest
```

```
switchbot scan                              # list Bots in range
switchbot                                   # scan, pick a Bot, then type commands
switchbot AA:BB:CC:DD:EE:FF                 # type commands for that Bot over one connection
switchbot AA:BB:CC:DD:EE:FF on              # run one command
switchbot AA:BB:CC:DD:EE:FF mode switch
switchbot -v AA:BB:CC:DD:EE:FF status       # log every step and every Bluetooth device seen
```

The commands are `on`, `off`, `press`, `down`, `up`, `status`, `mode press|switch`, `invert on|off` and `hold <seconds>`. The flags are `-t` (scan time), `-p` (password) and `-v` (verbose).

## Example

[`examples/onoff`](examples/onoff/main.go) shows a Bot's status, turns it on, waits three seconds and turns it off, timing each command:

```
go run ./examples/onoff -addr AA:BB:CC:DD:EE:FF
```

## Platform notes

- **Windows 10/11**: works out of the box. Bots usually don't advertise their type, so `DiscoverBots` connects to each SwitchBot device briefly to check it.
- **Linux**: needs BlueZ (`bluetoothd`). You may also need to add your user to the `bluetooth` group.
- **macOS**: needs cgo (Xcode command-line tools). macOS hides MAC addresses, so a Bot's address is a UUID. Use the one `DiscoverBots` or `switchbot scan` reports.

## Troubleshooting

- **No Bots found / `ErrNotFound`**: move closer and close the SwitchBot phone app. A Bot accepts only one connection, and stops advertising while something is connected to it.
- **Slow or failing connections**: check `bot.RSSI()`. Below about -85 dBm the Bot is near the edge of range.
- **See what's happening**: pass `WithLogger`, or use `switchbot -v`. You'll see each scan, connection, and the bytes sent and received.

## How it works

The protocol follows [SwitchBot's BLE API documentation](https://github.com/OpenWonderLabs/SwitchBotAPI-BLE) and was checked against real Bots running firmware 6.8. Password support uses the format the SwitchBot app sends to password-protected Bots (the CRC-32 of the password). SwitchBot's public documentation doesn't describe it.

## Contributing

Issues and pull requests are welcome. Before sending a change, please run:

```
gofmt -l .
go vet ./...
go test ./...
```

The tests use a simulated Bluetooth stack, so they don't need a Bot.

## License

[MIT](LICENSE)

This project isn't affiliated with or endorsed by SwitchBot or Woan Technology. "SwitchBot" is a trademark of its owner.
