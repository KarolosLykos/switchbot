// Package switchbot controls SwitchBot Bots directly over Bluetooth LE.
//
// No SwitchBot Hub, cloud account or internet connection is needed: the
// computer only has to be within Bluetooth range of the Bot. It works on
// Windows 10/11, Linux (BlueZ) and macOS.
//
// # Usage
//
// Create a [Client], get a [Bot] and send it commands:
//
//	client, err := switchbot.NewClient()
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer client.Close()
//
//	// If you know the Bot's address:
//	bot, err := client.Bot("AA:BB:CC:DD:EE:FF")
//
//	// Otherwise scan for 10 seconds and get every Bot in range:
//	bots, err := client.DiscoverBots(ctx, 10*time.Second)
//
//	err = bot.TurnOn(ctx)
//	err = bot.TurnOff(ctx)
//	status, err := bot.Status(ctx)
//
// # Connections
//
// A Bot connects on its first command and stays connected, so later
// commands are fast (about 0.1 s instead of several seconds). If the
// connection drops, the next command reconnects by itself. After a minute
// without commands the Bot is disconnected, so it doesn't drain its battery
// or block the SwitchBot app; change this with [WithIdleTimeout]. Call
// [Bot.Close] or [Client.Close] to disconnect right away.
//
// The first connection also checks that the device really is a Bot; if not,
// commands return [ErrNotABot].
//
// # Cancellation
//
// Every method that talks to a Bot takes a context, used only for
// cancellation: if it is cancelled, the method stops and returns ctx.Err().
// Scan length is an explicit argument of [Client.DiscoverBots].
//
// # Errors
//
// When a Bot rejects a command, the error is a [*StatusError] holding the
// Bot's status code. Common cases can be checked with errors.Is, for
// example errors.Is(err, switchbot.ErrUnsupported) or
// errors.Is(err, switchbot.ErrPasswordRequired).
//
// # Concurrency
//
// A Client and its Bots are safe for concurrent use. Commands to one Bot run
// one at a time; different Bots can be controlled at the same time.
//
// # Protocol
//
// The protocol is described in SwitchBot's BLE API documentation:
// https://github.com/OpenWonderLabs/SwitchBotAPI-BLE
package switchbot
