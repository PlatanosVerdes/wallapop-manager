// Package commands is the ear of the bot: it owns the updates, ignores everybody who is
// not the owner of the chat, and answers the handful of questions worth asking from the
// phone.
//
// Names are prefixed on purpose. The command menu belongs to the bot and not to this
// service, so the day another one joins, its commands sit next to these without a clash
// and without renaming anything here.
package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/telegram"
)

// Prefix is the service's corner of a shared bot: commands are wp_something, and the data
// a button sends back starts with wp:. Anything else belongs to another service.
const (
	Prefix     = "wp_"
	DataPrefix = "wp:"
)

type Command struct {
	// Name carries the prefix and no slash: "wp_status".
	Name string
	Help string
	Run  func(ctx context.Context) (Reply, error)
}

// Reply is what a command answers. Text is HTML, because a message read on a phone needs
// weight and not columns: whoever builds it escapes what came from a stranger.
type Reply struct {
	Text string
	Keys *telegram.Keyboard
}

func Say(text string) Reply { return Reply{Text: text} }

type Listener struct {
	Bot *telegram.Bot
	// Chat is the only conversation obeyed. A bot is public: anybody who finds it can
	// write to it, and nobody else gets an answer.
	Chat     string
	Commands []Command
	// OnButton answers a press. The notice is the banner raised on the phone, and a
	// keyboard that comes back redraws the one that was pressed.
	OnButton func(ctx context.Context, data string) (notice string, keys *telegram.Keyboard, err error)
	Log      *slog.Logger
	// Backoff is the wait after a failed poll.
	Backoff time.Duration
	// StaleAfter is how old a message may be and still be acted on. It exists for the
	// restart: the queue handed over on the first read holds whatever was sent while the
	// process was down, and this service is redeployed often enough that a command sent
	// seconds before a restart is one the sender is still waiting for, while one sent
	// this morning is not.
	StaleAfter time.Duration
}

// Serve reads updates until the context is done. It is the only place in the service that
// calls getUpdates, because Telegram gives an update to one reader and refuses a second.
func (l *Listener) Serve(ctx context.Context) error {
	if l.Bot == nil || !l.Bot.Enabled() {
		return nil
	}
	if l.Backoff == 0 {
		l.Backoff = 10 * time.Second
	}
	if l.StaleAfter == 0 {
		l.StaleAfter = 2 * time.Minute
	}

	if err := l.Bot.SetCommands(ctx, l.menu()); err != nil {
		l.Log.Warn("could not publish the command menu", "err", err)
	}

	// Start from the last update rather than from whatever is queued, and judge what comes
	// back by its age: a command sent seconds before a restart still deserves an answer,
	// one sent this morning does not.
	offset, first := int64(-1), true
	for {
		updates, err := l.Bot.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			l.Log.Error("could not read the updates", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(l.Backoff):
			}
			continue
		}

		for _, update := range updates {
			offset = update.UpdateID + 1
			switch {
			case update.CallbackQuery != nil:
				// A press carries no time of its own, so an old one cannot be told from a
				// recent one. The queue found on the first read is left alone rather than
				// silencing a search hours after somebody asked: a press lost to a
				// restart is pressed again, and the button shows which way it went.
				if first {
					continue
				}
				l.press(ctx, *update.CallbackQuery)
			case update.Message != nil:
				l.handle(ctx, *update.Message)
			}
		}
		first = false
	}
}

func (l *Listener) menu() []telegram.Command {
	menu := make([]telegram.Command, 0, len(l.Commands))
	for _, cmd := range l.Commands {
		menu = append(menu, telegram.Command{Name: cmd.Name, Description: cmd.Help})
	}
	return menu
}

func (l *Listener) handle(ctx context.Context, msg telegram.Message) {
	if strconv.FormatInt(msg.Chat.ID, 10) != l.Chat {
		l.Log.Warn("a message from another chat was ignored", "chat", msg.Chat.ID)
		return
	}

	name := parse(msg.Text)
	if name == "" {
		return
	}
	// The lag is worth a number: what is felt as a slow bot is usually a command sent
	// while the container was being replaced.
	lag := time.Duration(0)
	if msg.Date > 0 {
		lag = time.Since(time.Unix(msg.Date, 0)).Round(time.Second)
		if lag > l.StaleAfter {
			l.Log.Info("stale command dropped", "name", name, "lag", lag)
			return
		}
	}
	for _, cmd := range l.Commands {
		if cmd.Name != name {
			continue
		}
		l.Log.Info("command", "name", name, "lag", lag)
		reply, err := cmd.Run(ctx)
		if err != nil {
			reply = Reply{Text: "⚠️ no ha podido ser: " + telegram.Escape(err.Error())}
		}
		if err := l.Bot.Text(ctx, reply.Text, reply.Keys); err != nil {
			l.Log.Error("could not answer", "command", name, "err", err)
		}
		return
	}

	// Anything else belongs to somebody else, or to nobody. Answering it would make a
	// shared bot argue with itself.
	if strings.HasPrefix(name, Prefix) {
		l.Log.Info("unknown command", "name", name)
	}
}

// press deals with a button. Telegram leaves the phone spinning until the query is
// answered, so every path out of here answers it.
func (l *Listener) press(ctx context.Context, query telegram.CallbackQuery) {
	if query.Message == nil || strconv.FormatInt(query.Message.Chat.ID, 10) != l.Chat {
		l.Log.Warn("a button press from another chat was ignored")
		_ = l.Bot.Answer(ctx, query.ID, "")
		return
	}
	if !strings.HasPrefix(query.Data, DataPrefix) || l.OnButton == nil {
		_ = l.Bot.Answer(ctx, query.ID, "")
		return
	}

	l.Log.Info("button", "data", query.Data)
	notice, keys, err := l.OnButton(ctx, query.Data)
	if err != nil {
		notice = "no ha podido ser: " + err.Error()
	}
	if err := l.Bot.Answer(ctx, query.ID, notice); err != nil {
		l.Log.Error("could not close the button press", "err", err)
	}
	if err == nil && keys != nil {
		if err := l.Bot.EditKeys(ctx, query.Message.MessageID, keys); err != nil {
			l.Log.Error("could not redraw the buttons", "err", err)
		}
	}
}

// parse pulls the command out of a message: the first word, without the slash and without
// the @bot suffix a group chat adds.
func parse(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	name := strings.Fields(text)[0][1:]
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return strings.ToLower(name)
}

// Help is the answer to the help command, built from the table so it cannot drift from it.
func Help(commands []Command) string {
	var b strings.Builder
	b.WriteString("🤖 <b>Lo que entiendo</b>\n\n")
	for _, cmd := range commands {
		fmt.Fprintf(&b, "/%s\n<i>%s</i>\n", cmd.Name, telegram.Escape(cmd.Help))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ErrBusy is what a command answers when the round it would start is already running.
var ErrBusy = errors.New("ya hay una ronda en marcha")
