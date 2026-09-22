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

// Reply is what a command answers: the text, and the buttons it hangs from when there is
// something to press.
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

	if err := l.Bot.SetCommands(ctx, l.menu()); err != nil {
		l.Log.Warn("could not publish the command menu", "err", err)
	}

	// Start from the last update rather than from whatever is queued: a restart must not
	// replay this morning's commands. The first read is only there to learn where the
	// queue ends, so nothing in it is run.
	offset, bootstrap := int64(-1), true
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
			if bootstrap {
				continue
			}
			switch {
			case update.CallbackQuery != nil:
				l.press(ctx, *update.CallbackQuery)
			case update.Message != nil:
				l.handle(ctx, *update.Message)
			}
		}
		bootstrap = false
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
	for _, cmd := range l.Commands {
		if cmd.Name != name {
			continue
		}
		l.Log.Info("command", "name", name)
		reply, err := cmd.Run(ctx)
		if err != nil {
			reply = Reply{Text: "no ha podido ser: " + err.Error()}
		}
		if err := l.Bot.Text(ctx, telegram.Escape(reply.Text), reply.Keys); err != nil {
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
	b.WriteString("Lo que entiendo:\n")
	for _, cmd := range commands {
		fmt.Fprintf(&b, "/%s — %s\n", cmd.Name, cmd.Help)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ErrBusy is what a command answers when the round it would start is already running.
var ErrBusy = errors.New("ya hay una ronda en marcha")
