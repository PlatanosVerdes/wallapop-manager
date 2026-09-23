// Package commands is the ear of the bot: it owns the updates, lets through the chats the
// gate knows, and answers the handful of questions worth asking from the phone.
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

type Command struct {
	// Name has no slash: "estado".
	Name string
	Help string
	// Open commands answer anybody. The rest answer only the chats the gate lets in.
	Open bool
	Run  func(ctx context.Context, req Request) (Reply, error)
}

// Request is who asked, and whatever was written after the command.
type Request struct {
	Chat telegram.Chat
	Args string
	// Location is set when what was sent is a place and not text.
	Location *telegram.Location
}

func (r Request) ChatID() string { return strconv.FormatInt(r.Chat.ID, 10) }

// Reply is what a command answers. Text is HTML, because a message read on a phone needs
// weight and not columns: whoever builds it escapes what came from a stranger.
type Reply struct {
	Text string
	Keys *telegram.Keyboard
}

func Say(text string) Reply { return Reply{Text: text} }

type Listener struct {
	Bot *telegram.Bot
	// Allowed is the gate. A bot is public: anybody who finds it can write to it, and only
	// the chats this lets through get more than the open commands.
	Allowed  func(chat string) bool
	Commands []Command
	// OnText answers a message from an allowed chat that is not a command.
	OnText func(ctx context.Context, req Request) (Reply, error)
	// OnButton answers a press. The notice is the banner raised on the phone, and a
	// keyboard that comes back redraws the one that was pressed.
	OnButton func(ctx context.Context, chat, data string) (notice string, keys *telegram.Keyboard, err error)
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

func (l *Listener) allowed(chat string) bool { return l.Allowed != nil && l.Allowed(chat) }

func (l *Listener) handle(ctx context.Context, msg telegram.Message) {
	chat := strconv.FormatInt(msg.Chat.ID, 10)
	name, args := parse(msg.Text)
	req := Request{Chat: msg.Chat, Args: args, Location: msg.Location}

	// The lag is worth a number: what is felt as a slow bot is usually a command sent
	// while the container was being replaced.
	lag := time.Duration(0)
	if msg.Date > 0 {
		lag = time.Since(time.Unix(msg.Date, 0)).Round(time.Second)
		if lag > l.StaleAfter {
			l.Log.Info("stale message dropped", "name", name, "lag", lag)
			return
		}
	}

	if name == "" {
		if l.OnText == nil || !l.allowed(chat) {
			return
		}
		l.answer(ctx, chat, "text", func() (Reply, error) { return l.OnText(ctx, req) })
		return
	}

	for _, cmd := range l.Commands {
		if cmd.Name != name {
			continue
		}
		if !cmd.Open && !l.allowed(chat) {
			l.Log.Warn("a command from a chat not let in was ignored", "chat", chat, "name", name)
			return
		}
		l.Log.Info("command", "name", name, "chat", chat, "lag", lag)
		l.answer(ctx, chat, name, func() (Reply, error) { return cmd.Run(ctx, req) })
		return
	}
	if l.allowed(chat) {
		l.Log.Info("unknown command", "name", name)
		_ = l.Bot.To(chat).Text(ctx, "No conozco ese comando. /ayuda", nil)
	}
}

func (l *Listener) answer(ctx context.Context, chat, what string, run func() (Reply, error)) {
	reply, err := run()
	if err != nil {
		reply = Reply{Text: "⚠️ " + telegram.Escape(err.Error())}
	}
	if reply.Text == "" {
		return
	}
	if err := l.Bot.To(chat).Text(ctx, reply.Text, reply.Keys); err != nil {
		l.Log.Error("could not answer", "what", what, "chat", chat, "err", err)
	}
}

// press deals with a button. Telegram leaves the phone spinning until the query is
// answered, so every path out of here answers it.
func (l *Listener) press(ctx context.Context, query telegram.CallbackQuery) {
	if query.Message == nil || !l.allowed(strconv.FormatInt(query.Message.Chat.ID, 10)) {
		l.Log.Warn("a button press from a chat not let in was ignored")
		_ = l.Bot.Answer(ctx, query.ID, "")
		return
	}
	if l.OnButton == nil {
		_ = l.Bot.Answer(ctx, query.ID, "")
		return
	}

	chat := strconv.FormatInt(query.Message.Chat.ID, 10)
	l.Log.Info("button", "data", query.Data, "chat", chat)
	notice, keys, err := l.OnButton(ctx, chat, query.Data)
	if err != nil {
		notice = "no ha podido ser: " + err.Error()
	}
	if err := l.Bot.Answer(ctx, query.ID, notice); err != nil {
		l.Log.Error("could not close the button press", "err", err)
	}
	if err == nil && keys != nil {
		if err := l.Bot.To(chat).EditKeys(ctx, query.Message.MessageID, keys); err != nil {
			l.Log.Error("could not redraw the buttons", "err", err)
		}
	}
}

// parse pulls the command out of a message: the first word, without the slash and without
// the @bot suffix a group chat adds, and the rest as its arguments. A message that is not a
// command comes back whole as the arguments.
func parse(text string) (name, args string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	first, rest, _ := strings.Cut(text, " ")
	name = first[1:]
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return strings.ToLower(name), strings.TrimSpace(rest)
}

// Help is the answer to the help command, built from the table so it cannot drift from it.
func Help(commands []Command) string {
	var b strings.Builder
	for _, cmd := range commands {
		fmt.Fprintf(&b, "/%s · %s\n", cmd.Name, telegram.Escape(cmd.Help))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ErrBusy is what a command answers when the round it would start is already running.
var ErrBusy = errors.New("ya estoy buscando, prueba en un momento")
