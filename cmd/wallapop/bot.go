package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
}

func (b *botState) commands(listener *commands.Listener) []commands.Command {
	return []commands.Command{
		{
			Name: "start",
			Help: "darte de alta",
			Open: true,
			Run:  b.start,
		},
		{
			Name: "nueva",
			Help: "guarda una busqueda: un nombre si quieres y la direccion de una busqueda hecha en la web",
			Run: func(_ context.Context, req commands.Request) (commands.Reply, error) {
				return b.onText(context.Background(), req)
			},
		},
		{
			Name: "busquedas",
			Help: "tus busquedas, para silenciarlas o eliminarlas",
			Run: func(_ context.Context, req commands.Request) (commands.Reply, error) {
				user, _ := b.people.Get(req.ChatID())
				return commands.Reply{Text: searchesText(user, b.cfg.MaxSearches), Keys: searchKeys(user)}, nil
			},
		},
		{
			Name: "ahora",
			Help: "mira tus busquedas ahora, sin esperar al reloj",
			Run: func(ctx context.Context, req commands.Request) (commands.Reply, error) {
				// A command would rather be told no than queue behind a round that is
				// already doing the very thing it asked for.
				if !watching.TryLock() {
					return commands.Reply{}, commands.ErrBusy
				}
				defer watching.Unlock()
				// The new listings announce themselves; this is only the receipt.
				res := runWatch(ctx, b.cfg, b.people, b.log, round{Chat: req.ChatID()})
				return commands.Say(htmlRound(res)), nil
			},
		},
		{
			Name: "baja",
			Help: "borra tus busquedas y te da de baja",
			Run: func(context.Context, commands.Request) (commands.Reply, error) {
				return commands.Reply{
					Text: "¿Te doy de baja? Se borran tus busquedas y lo que he visto para ti.",
					Keys: &telegram.Keyboard{Rows: [][]telegram.Button{{
						{Text: "Si, dame de baja", Data: buttonLeave, Style: "danger"},
						{Text: "No", Data: buttonStay},
					}}},
				}, nil
			},
		},
		{
			Name: "ayuda",
			Help: "esto",
			Run: func(context.Context, commands.Request) (commands.Reply, error) {
				return commands.Say(commands.Help(listener.Commands) + "\n\n" + howToAdd), nil
			},
		},
	}
}

const howToAdd = "<i>Para guardar una busqueda, hazla en es.wallapop.com con los filtros que quieras " +
	"y pegame aqui la direccion de la pagina. Si escribes algo al lado, por ejemplo " +
	"\"coches top\" y la direccion, la busqueda se llama asi.</i>"

// start is the only command a stranger can run, and all it takes to join.
func (b *botState) start(_ context.Context, req commands.Request) (commands.Reply, error) {
	chat, name := req.ChatID(), req.Chat.Name()
	if _, ok := b.people.Get(chat); ok {
		if err := b.people.Rename(chat, name); err != nil {
			return commands.Reply{}, err
		}
		return commands.Say("👋 Ya estas dentro.\n\n" + howToAdd), nil
	}

	if len(b.people.All()) >= b.cfg.MaxUsers {
		b.log.Warn("somebody could not join, the bot is full", "chat", chat, "name", name)
		return commands.Say("Lo siento, el bot esta lleno."), nil
	}
	if _, err := b.people.Request(chat, name, true, time.Now()); err != nil {
		return commands.Reply{}, err
	}
	b.log.Info("a chat joined", "chat", chat, "name", name)
	return commands.Say("👋 Hola. " + howToAdd + "\n\n/ayuda para el resto."), nil
}

// onText takes a pasted address as a new search, which is the whole of adding one from a
// phone: copy the page, paste it here. Whatever is written around the address, before or
// after it, is the name.
func (b *botState) onText(_ context.Context, req commands.Request) (commands.Reply, error) {
	for _, word := range strings.Fields(req.Args) {
		if strings.Contains(word, "wallapop.com") {
			name := strings.Join(strings.Fields(strings.Replace(req.Args, word, "", 1)), " ")
			return b.add(req.ChatID(), word, name)
		}
	}
	return commands.Say(howToAdd), nil
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
		fmt.Fprintf(&t, "📍 a %s km como mucho\n", telegram.Escape(km))
	} else {
		t.WriteString("📍 toda España\n")
	}
	if price := priceRange(query.Get("min_sale_price"), query.Get("max_sale_price")); price != "" {
		fmt.Fprintf(&t, "💶 %s\n", price)
	}
	fmt.Fprintf(&t, "\n<i>Lo que ya esta publicado lo apunto sin avisar; desde ahora te llega lo nuevo. "+
		"Llevas %d de %d.</i>", len(user.Searches), b.cfg.MaxSearches)
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
		return "Silenciada " + search.Name + ". Se enciende otra vez desde /busquedas", done, nil

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
			return commands.ErrBusy.Error() + ", prueba en un minuto", nil, nil
		}
		defer watching.Unlock()
		if err := b.people.Remove(chat); err != nil {
			return "", nil, err
		}
		if err := os.RemoveAll(userDir(b.cfg, chat)); err != nil {
			b.log.Error("could not remove what was seen for a chat that left", "chat", chat, "err", err)
		}
		b.log.Info("a chat left", "chat", chat)
		return "Hecho", &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off("👋 Dado de baja")}}}, nil

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
		return "🔎 <b>No tienes busquedas</b>\n\n" + howToAdd
	}
	muted := 0
	for _, search := range user.Searches {
		if search.Muted {
			muted++
		}
	}
	var t strings.Builder
	fmt.Fprintf(&t, "🔎 <b>Tus busquedas</b> · %d de %d\n", len(user.Searches), limit)
	if muted > 0 {
		fmt.Fprintf(&t, "%d silenciadas\n", muted)
	}
	t.WriteString("<i>la campana silencia o devuelve, la papelera elimina</i>")
	return t.String()
}

// htmlRound is what a round looks like when it is read rather than logged: the numbers
// that changed in bold, the rest as context.
func htmlRound(res watch.Result) string {
	if res.Error != "" {
		return "⚠️ <b>La ronda ha fallado</b>\n" + telegram.Escape(res.Error)
	}

	var t strings.Builder
	fmt.Fprintf(&t, "🔎 <b>Ronda de las %s</b>\n", res.StartedAt.Format("15:04"))
	fmt.Fprintf(&t, "%d anuncios · <b>%d nuevos</b> · %d repetidos\n", res.Scanned, len(res.New), res.Duplicates)
	if len(res.Cheaper) > 0 {
		fmt.Fprintf(&t, "<b>%d han bajado de precio</b>\n", len(res.Cheaper))
	}
	fmt.Fprintf(&t, "%d busquedas vigiladas", res.Watched)
	if res.Silenced > 0 {
		fmt.Fprintf(&t, ", %d silenciadas", res.Silenced)
	}
	if res.Seeded > 0 {
		fmt.Fprintf(&t, "\n<i>%d apuntados sin avisar: ya estaban ahi</i>", res.Seeded)
	}
	for _, f := range res.Failures {
		fmt.Fprintf(&t, "\n⚠️ %s: %s", telegram.Escape(f.Search), telegram.Escape(f.Error))
	}
	return t.String()
}
