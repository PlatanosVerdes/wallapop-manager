package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/commands"
	"github.com/PlatanosVerdes/wallapop-manager/internal/config"
	"github.com/PlatanosVerdes/wallapop-manager/internal/telegram"
	"github.com/PlatanosVerdes/wallapop-manager/internal/users"
	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
	"github.com/PlatanosVerdes/wallapop-manager/internal/watch"
)

// What a button sends back, by what it does. Telegram caps callback data at 64 bytes, so
// the verb is one letter and the rest is a search id or a chat id.
const (
	buttonToggle    = "t:"
	buttonMute      = "m:"
	buttonAskDelete = "d:"
	buttonRename    = "e:"
	buttonCheck     = "n:"
	buttonDelete    = "D:"
	buttonKeep      = "k:"
	buttonLeave     = "B:"
	buttonStay      = "x:"
)

// botState answers each chat about its own searches and nothing else: no other chat, and
// nothing of the owner's account.
type botState struct {
	cfg    config.Config
	people *users.Store
	log    *slog.Logger
	bot    *telegram.Bot

	// renaming is the search each chat was asked a new name for. It lives in memory: a
	// question lost to a restart is asked again by pressing the pencil.
	mu       sync.Mutex
	renaming map[string]renaming
}

type renaming struct {
	search string
	asked  time.Time
}

// renameWindow is how long the next message is taken as the new name.
const renameWindow = 5 * time.Minute

// nameLimit keeps a name short enough to fit on a button next to two others.
const nameLimit = 40

// pendingRename answers the search a chat is naming, and forgets the question either way.
func (b *botState) pendingRename(chat string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.renaming[chat]
	delete(b.renaming, chat)
	return r.search, ok && time.Since(r.asked) < renameWindow
}

func (b *botState) commands(listener *commands.Listener) []commands.Command {
	return []commands.Command{
		{
			Name: "start",
			Help: "Empezar",
			Open: true,
			Run:  b.start,
		},
		{
			Name: "nueva",
			Help: "Guardar una búsqueda",
			Run: func(_ context.Context, req commands.Request) (commands.Reply, error) {
				return b.onText(context.Background(), req)
			},
		},
		{
			Name: "busquedas",
			Help: "Ver y editar tus búsquedas",
			Run: func(_ context.Context, req commands.Request) (commands.Reply, error) {
				user, _ := b.people.Get(req.ChatID())
				return commands.Reply{Text: searchesText(user, b.cfg.MaxSearches), Keys: searchKeys(user)}, nil
			},
		},
		{
			Name: "ahora",
			Help: "Buscar ahora",
			Run: func(ctx context.Context, req commands.Request) (commands.Reply, error) {
				user, _ := b.people.Get(req.ChatID())
				switch len(user.Searches) {
				case 0:
					return commands.Say("🔎 <b>Aún no tienes búsquedas</b>\n\n" + howToAdd), nil
				case 1:
					text, err := b.check(ctx, req.ChatID(), user.Searches[0].ID)
					return commands.Say(text), err
				}
				return commands.Reply{Text: "🔎 ¿Qué busco?", Keys: checkKeys(user)}, nil
			},
		},
		{
			Name: "baja",
			Help: "Darte de baja",
			Run: func(context.Context, commands.Request) (commands.Reply, error) {
				return commands.Reply{
					Text: "¿Te doy de baja? Se borrarán tus búsquedas.",
					Keys: &telegram.Keyboard{Rows: [][]telegram.Button{{
						{Text: "Sí, darme de baja", Data: buttonLeave, Style: "danger"},
						{Text: "No", Data: buttonStay},
					}}},
				}, nil
			},
		},
		{
			Name: "ayuda",
			Help: "Cómo funciona",
			Run: func(context.Context, commands.Request) (commands.Reply, error) {
				return commands.Say(intro + "\n\n" + howToAdd + "\n\n" + commands.Help(listener.Commands)), nil
			},
		},
	}
}

const (
	intro    = "Te aviso cuando sale algo nuevo en tus búsquedas de Wallapop."
	howToAdd = "<i>Haz la búsqueda en es.wallapop.com y pégame el enlace. " +
		"Si le pones un nombre delante, se llama así.</i>"
)

// start is the only command a stranger can run, and all it takes to join.
func (b *botState) start(_ context.Context, req commands.Request) (commands.Reply, error) {
	chat, name := req.ChatID(), req.Chat.Name()
	if _, ok := b.people.Get(chat); ok {
		if err := b.people.Rename(chat, name); err != nil {
			return commands.Reply{}, err
		}
		return commands.Say("👋 Ya estás dentro.\n\n" + howToAdd), nil
	}

	if len(b.people.All()) >= b.cfg.MaxUsers {
		b.log.Warn("somebody could not join, the bot is full", "chat", chat, "name", name)
		return commands.Say("El bot está lleno, lo siento."), nil
	}
	if _, err := b.people.Request(chat, name, true, time.Now()); err != nil {
		return commands.Reply{}, err
	}
	b.log.Info("a chat joined", "chat", chat, "name", name)
	return commands.Say("👋 ¡Hola! " + intro + "\n\n" + howToAdd), nil
}

// onText takes a pasted address as a new search, which is the whole of adding one from a
// phone: copy the page, paste it here. Whatever is written around the address, before or
// after it, is the name.
func (b *botState) onText(_ context.Context, req commands.Request) (commands.Reply, error) {
	id, renamingOne := b.pendingRename(req.ChatID())
	for _, word := range strings.Fields(req.Args) {
		if strings.Contains(word, "wallapop.com") {
			name := strings.Join(strings.Fields(strings.Replace(req.Args, word, "", 1)), " ")
			return b.add(req.ChatID(), word, name)
		}
	}
	if renamingOne {
		return b.rename(req.ChatID(), id, req.Args)
	}
	return commands.Say(howToAdd), nil
}

func (b *botState) rename(chat, id, name string) (commands.Reply, error) {
	name = strings.Join(strings.Fields(name), " ")
	if runes := []rune(name); len(runes) > nameLimit {
		name = string(runes[:nameLimit])
	}
	if name == "" {
		return commands.Say("Necesito un nombre. Pulsa ✏️ otra vez en /busquedas."), nil
	}
	search, err := b.people.RenameSearch(chat, id, name)
	if err != nil {
		return commands.Reply{}, err
	}
	return commands.Say("✏️ Ahora se llama <b>" + telegram.Escape(search.Name) + "</b>"), nil
}

func (b *botState) add(chat, address, name string) (commands.Reply, error) {
	search, err := addSearch(b.cfg, b.people, chat, address, name)
	if err != nil {
		return commands.Reply{}, err
	}
	user, _ := b.people.Get(chat)

	query := search.Values()
	var t strings.Builder
	fmt.Fprintf(&t, "✅ Guardada <b>%s</b>\n", telegram.Escape(search.Name))
	if km := wallapop.RadiusKm(query); km != "" {
		fmt.Fprintf(&t, "📍 hasta %s km\n", telegram.Escape(km))
	} else {
		t.WriteString("📍 toda España\n")
	}
	if price := priceRange(query.Get("min_sale_price"), query.Get("max_sale_price")); price != "" {
		fmt.Fprintf(&t, "💶 %s\n", price)
	}
	fmt.Fprintf(&t, "\n<i>Te aviso de lo nuevo a partir de ahora (%d/%d)</i>", len(user.Searches), b.cfg.MaxSearches)
	return commands.Reply{Text: t.String(), Keys: &telegram.Keyboard{Rows: [][]telegram.Button{{
		{Text: "🔗 Ver en Wallapop", URL: wallapop.WebURL(query)},
	}}}}, nil
}

func priceRange(min, max string) string {
	switch {
	case min != "" && max != "":
		return fmt.Sprintf("de %s a %s €", min, max)
	case max != "":
		return fmt.Sprintf("hasta %s €", max)
	case min != "":
		return fmt.Sprintf("desde %s €", min)
	}
	return ""
}

// onButton is everything a press can do. The chat is the one the press came from, and
// every search is looked up inside it, so nobody's button reaches somebody else's search.
func (b *botState) onButton(ctx context.Context, chat, data string) (string, *telegram.Keyboard, error) {
	verb, arg := data, ""
	if len(data) >= 2 {
		verb, arg = data[:2], data[2:]
	}

	switch verb {
	case buttonToggle:
		search, err := b.people.Search(chat, arg)
		if err != nil {
			return err.Error(), nil, nil
		}
		search, err = b.people.SetMuted(chat, arg, !search.Muted)
		if err != nil {
			return "", nil, err
		}
		notice := "Vuelvo a avisarte de " + search.Name
		if search.Muted {
			notice = "Silenciada " + search.Name
		}
		return notice, b.keysOf(chat), nil

	case buttonMute:
		search, err := b.people.SetMuted(chat, arg, true)
		if err != nil {
			return err.Error(), nil, nil
		}
		// The button that did it becomes the label saying it is done.
		done := &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off("🔕 " + search.Name + " silenciada")}}}
		return "Silenciada. Se reactiva en /busquedas", done, nil

	case buttonRename:
		search, err := b.people.Search(chat, arg)
		if err != nil {
			return err.Error(), nil, nil
		}
		b.mu.Lock()
		if b.renaming == nil {
			b.renaming = map[string]renaming{}
		}
		b.renaming[chat] = renaming{search: search.ID, asked: time.Now()}
		b.mu.Unlock()
		ask := "✏️ ¿Qué nombre le pongo a <b>" + telegram.Escape(search.Name) + "</b>?"
		if b.bot != nil {
			if err := b.bot.To(chat).Text(ctx, ask, nil); err != nil {
				return "", nil, err
			}
		}
		return "Escríbeme el nombre", nil, nil

	case buttonCheck:
		text, err := b.check(ctx, chat, arg)
		if err != nil {
			return err.Error(), nil, nil
		}
		return "", &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off(text)}}}, nil

	case buttonAskDelete:
		search, err := b.people.Search(chat, arg)
		if err != nil {
			return err.Error(), nil, nil
		}
		return "¿Eliminar " + search.Name + "?", &telegram.Keyboard{Rows: [][]telegram.Button{{
			{Text: "🗑 Eliminar " + search.Name, Data: buttonDelete + search.ID, Style: "danger"},
			{Text: "No", Data: buttonKeep},
		}}}, nil

	case buttonDelete:
		search, err := b.people.Delete(chat, arg)
		if err != nil {
			return err.Error(), b.keysOf(chat), nil
		}
		b.log.Info("search deleted", "chat", chat, "search", search.Name)
		return "Eliminada " + search.Name, b.keysOf(chat), nil

	case buttonKeep:
		return "", b.keysOf(chat), nil

	case buttonLeave:
		// A round in progress would write this chat's folder back after it is removed.
		if !watching.TryLock() {
			return commands.ErrBusy.Error(), nil, nil
		}
		defer watching.Unlock()
		if err := b.people.Remove(chat); err != nil {
			return "", nil, err
		}
		if err := os.RemoveAll(userDir(b.cfg, chat)); err != nil {
			b.log.Error("could not remove what was seen for a chat that left", "chat", chat, "err", err)
		}
		b.log.Info("a chat left", "chat", chat)
		return "Hecho", &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off("👋 Hecho, hasta pronto")}}}, nil

	case buttonStay:
		return "", &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off("Sigues dentro")}}}, nil
	}
	return "", nil, nil
}

func (b *botState) keysOf(chat string) *telegram.Keyboard {
	user, _ := b.people.Get(chat)
	if keys := searchKeys(user); keys != nil {
		return keys
	}
	return &telegram.Keyboard{}
}

// check runs one of a chat's searches now, or all of them when id is empty. The new
// listings announce themselves; what comes back is only the receipt.
func (b *botState) check(ctx context.Context, chat, id string) (string, error) {
	name := "Todas"
	if id != "" {
		search, err := b.people.Search(chat, id)
		if err != nil {
			return "", err
		}
		name = search.Name
	}
	// A command would rather be told no than queue behind a round that is already doing
	// the very thing it asked for.
	if !watching.TryLock() {
		return "", commands.ErrBusy
	}
	defer watching.Unlock()
	res := runWatch(ctx, b.cfg, b.people, b.log, round{Chat: chat, Search: id})
	if res.Error != "" {
		return "", fmt.Errorf("no he podido buscar: %s", res.Error)
	}
	text := fmt.Sprintf("🔎 %s: %d mirados · %d nuevos", name, res.Scanned, len(res.New))
	if len(res.Cheaper) > 0 {
		text += fmt.Sprintf(" · %d más baratos", len(res.Cheaper))
	}
	if len(res.Failures) > 0 {
		text += " · ⚠️ " + res.Failures[0].Error
	}
	return text, nil
}

// checkKeys offers each search to run now, and all of them at once.
func checkKeys(user users.User) *telegram.Keyboard {
	keys := &telegram.Keyboard{}
	for _, search := range user.Searches {
		text := "🔎 " + search.Name
		if search.Muted {
			text = "🔕 " + search.Name
		}
		keys.Rows = append(keys.Rows, []telegram.Button{{Text: text, Data: buttonCheck + search.ID}})
	}
	keys.Rows = append(keys.Rows, []telegram.Button{{Text: "Todas", Data: buttonCheck, Style: "primary"}})
	return keys
}

// listingKeys puts the two things a listing is for under it: opening it, and hearing less
// of that search.
func listingKeys(search watch.Search, item wallapop.SearchItem) *telegram.Keyboard {
	return &telegram.Keyboard{Rows: [][]telegram.Button{{
		{Text: "🔗 Ver anuncio", URL: item.URL(), Style: "primary"},
		{Text: "🔕 Silenciar", Data: buttonMute + search.ID, Style: "danger"},
	}}}
}

// searchKeys draws one row per search: the switch with its name, and the bin.
func searchKeys(user users.User) *telegram.Keyboard {
	keys := &telegram.Keyboard{}
	for _, search := range user.Searches {
		text, style := "🔔 "+search.Name, "success"
		if search.Muted {
			text, style = "🔕 "+search.Name, "danger"
		}
		keys.Rows = append(keys.Rows, []telegram.Button{
			{Text: text, Data: buttonToggle + search.ID, Style: style},
			{Text: "✏️", Data: buttonRename + search.ID},
			{Text: "🗑", Data: buttonAskDelete + search.ID},
		})
	}
	if len(keys.Rows) == 0 {
		return nil
	}
	return keys
}

func searchesText(user users.User, limit int) string {
	if len(user.Searches) == 0 {
		return "🔎 <b>Aún no tienes búsquedas</b>\n\n" + howToAdd
	}
	muted := 0
	for _, search := range user.Searches {
		if search.Muted {
			muted++
		}
	}
	var t strings.Builder
	fmt.Fprintf(&t, "🔎 <b>Tus búsquedas</b> (%d/%d)", len(user.Searches), limit)
	if muted > 0 {
		fmt.Fprintf(&t, " · %d silenciadas", muted)
	}
	t.WriteString("\n<i>🔔 silenciar · ✏️ renombrar · 🗑 eliminar</i>")
	return t.String()
}
