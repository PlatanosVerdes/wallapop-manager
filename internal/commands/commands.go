// Package commands reads the bot's updates and answers them.
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
	// Name has no slash: "busquedas".
	Name string
	Help string
	// Open commands answer anybody, not only the chats Allowed lets in.
	Open bool
	Run  func(ctx context.Context, req Request) (Reply, error)
}

type Request struct {
	Chat telegram.Chat
	Args string
	// Location is set when a place was sent instead of text.
	Location *telegram.Location
}

func (r Request) ChatID() string { return strconv.FormatInt(r.Chat.ID, 10) }

// Reply.Text is HTML: whoever builds it escapes what came from a stranger.
type Reply struct {
	Text string
	Keys *telegram.Keyboard
}

func Say(text string) Reply { return Reply{Text: text} }

type Listener struct {
	Bot *telegram.Bot
	// Allowed is the gate: a public bot hears from anybody.
	Allowed  func(chat string) bool
	Commands []Command
	OnText   func(ctx context.Context, req Request) (Reply, error)
	// OnButton's notice is the banner on the phone; returned keys redraw the pressed ones.
	OnButton func(ctx context.Context, chat, data string) (notice string, keys *telegram.Keyboard, err error)
	Log      *slog.Logger
	Backoff  time.Duration
	// StaleAfter drops older messages: after a restart the queue may hold hours-old ones.
	StaleAfter time.Duration
}

// Serve is the only getUpdates caller in the service.
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
				// A press has no timestamp, so presses queued before a restart are skipped.
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

	// A slow bot is usually a command sent while the container was being replaced.
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

// parse drops the slash and the @bot suffix group chats add; plain text comes back as args.
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

func Help(commands []Command) string {
	var b strings.Builder
	for _, cmd := range commands {
		fmt.Fprintf(&b, "/%s · %s\n", cmd.Name, telegram.Escape(cmd.Help))
	}
	return strings.TrimRight(b.String(), "\n")
}

var ErrBusy = errors.New("ya estoy buscando, prueba en un momento")
