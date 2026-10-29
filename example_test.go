package switchbot_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/karoloslykos/switchbot"
)

// Control a Bot whose address you know. The first command connects; the
// rest reuse the connection.
func Example() {
	client, err := switchbot.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	bot, err := client.Bot("AA:BB:CC:DD:EE:FF")
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	if err := bot.TurnOn(ctx); err != nil {
		log.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	if err := bot.TurnOff(ctx); err != nil {
		log.Fatal(err)
	}
}

// Find every Bot in range.
func ExampleClient_DiscoverBots() {
	client, err := switchbot.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	bots, err := client.DiscoverBots(context.Background(), 10*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	for _, bot := range bots {
		fmt.Println(bot.Address(), bot.RSSI())
	}
}

// Read a Bot's battery, firmware and settings.
func ExampleBot_Status() {
	client, err := switchbot.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	bot, err := client.Bot("AA:BB:CC:DD:EE:FF")
	if err != nil {
		log.Fatal(err)
	}
	st, err := bot.Status(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("battery %d%%, firmware %s, %s mode\n", st.Battery, st.Firmware, st.Mode)
}

// Put a Bot in switch mode, so that TurnOn and TurnOff work.
func ExampleBot_SetMode() {
	client, err := switchbot.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	bot, err := client.Bot("AA:BB:CC:DD:EE:FF")
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	if err := bot.SetMode(ctx, switchbot.ModeSwitch); err != nil {
		log.Fatal(err)
	}
	if err := bot.TurnOn(ctx); err != nil {
		log.Fatal(err)
	}
}

// Check for specific failures with errors.Is.
func ExampleStatusError() {
	client, err := switchbot.NewClient(switchbot.WithPassword("my-password"))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	bot, err := client.Bot("AA:BB:CC:DD:EE:FF")
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	err = bot.TurnOn(ctx)
	switch {
	case errors.Is(err, switchbot.ErrUnsupported):
		// The Bot is in press mode, where it only presses.
		err = bot.Press(ctx)
	case errors.Is(err, switchbot.ErrWrongPassword):
		log.Fatal("wrong password")
	}
	if err != nil {
		log.Fatal(err)
	}
}
