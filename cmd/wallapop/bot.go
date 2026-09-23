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
	"github.com/PlatanosVerdes/wallapop-manager/internal/reactivate"
	"github.com/PlatanosVerdes/wallapop-manager/internal/session"
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
	buttonApprove   = "a:"
	buttonReject    = "r:"
	buttonLeave     = "B:"
	buttonStay      = "x:"
)

type botState struct {
	cfg       config.Config
	store     *session.Store
	people    *users.Store
	log       *slog.Logger
	bot       *telegram.Bot
	nextWatch func() time.Time
	nextRun   func() time.Time
}

func (b *botState) owner(chat string) bool { return chat == b.cfg.TelegramChat }

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
			Help: "guarda una busqueda: /nueva y la direccion de una busqueda hecha en la web",
			Run: func(_ context.Context, req commands.Request) (commands.Reply, error) {
				if req.Args == "" {
					return commands.Say(howToAdd), nil
				}
				return b.add(req.ChatID(), req.Args)
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
			Name: "estado",
			Help: "ultima ronda y proxima",
			Run: func(_ context.Context, req commands.Request) (commands.Reply, error) {
				return commands.Say(b.status(req.ChatID())), nil
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
	"y pegame aqui la direccion de la pagina.</i>"

// start is the only command a stranger can run. It asks the owner, once, and the owner's
// yes is what lets the chat in.
func (b *botState) start(ctx context.Context, req commands.Request) (commands.Reply, error) {
	chat := req.ChatID()
	if user, ok := b.people.Get(chat); ok {
		if user.Active {
			return commands.Say("👋 Ya estas dentro.\n\n" + howToAdd), nil
		}
		return commands.Say("⏳ Tu solicitud sigue pendiente."), nil
	}

	created, err := b.people.Request(chat, req.Chat.Name(), false, time.Now())
	if err != nil || !created {
		return commands.Reply{}, err
	}
	b.log.Info("somebody asked to join", "chat", chat, "name", req.Chat.Name())
	ask := fmt.Sprintf("🙋 <b>%s</b> quiere usar el bot.", telegram.Escape(req.Chat.Name()))
	keys := &telegram.Keyboard{Rows: [][]telegram.Button{{
		{Text: "✅ Dejarle entrar", Data: buttonApprove + chat, Style: "success"},
		{Text: "Rechazar", Data: buttonReject + chat, Style: "danger"},
	}}}
	if err := b.bot.To(b.cfg.TelegramChat).Text(ctx, ask, keys); err != nil {
		return commands.Reply{}, err
	}
	return commands.Say("👋 Hola. He pedido que te dejen entrar; te aviso aqui en cuanto este."), nil
}

// onText takes a pasted address as a new search, which is the whole of adding one from a
// phone: copy the page, paste it here.
func (b *botState) onText(_ context.Context, req commands.Request) (commands.Reply, error) {
	for _, word := range strings.Fields(req.Args) {
		if strings.Contains(word, "wallapop.com") {
			name := strings.Join(strings.Fields(strings.Replace(req.Args, word, "", 1)), " ")
			return b.add(req.ChatID(), word+" "+name)
		}
	}
	return commands.Say(howToAdd), nil
}

// add takes an address and, optionally, a name after it.
func (b *botState) add(chat, args string) (commands.Reply, error) {
	address, name, _ := strings.Cut(strings.TrimSpace(args), " ")
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

	case buttonApprove, buttonReject:
		if !b.owner(chat) {
			return "", nil, nil
		}
		return b.decide(ctx, verb == buttonApprove, arg)

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

// decide is the owner's answer to somebody asking to join, and the asker hears it.
func (b *botState) decide(ctx context.Context, approve bool, chat string) (string, *telegram.Keyboard, error) {
	user, ok := b.people.Get(chat)
	if !ok {
		return "Esa solicitud ya no esta", &telegram.Keyboard{}, nil
	}
	to := b.bot.To(chat)
	if !approve {
		if err := b.people.Remove(chat); err != nil {
			return "", nil, err
		}
		_ = to.Text(ctx, "No te han dado acceso.", nil)
		return "Rechazado", &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off("❌ " + user.Name + " rechazado")}}}, nil
	}

	if _, err := b.people.Approve(chat); err != nil {
		return "", nil, err
	}
	b.log.Info("a chat was let in", "chat", chat, "name", user.Name)
	_ = to.Text(ctx, "✅ Ya estas dentro.\n\n"+howToAdd+"\n\n/ayuda para el resto.", nil)
	return "Dentro", &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off("✅ " + user.Name + " dentro")}}}, nil
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

// status answers "is this alive". Everybody gets the rounds; the owner also gets the
// catalogue, the session and who is using the bot, which are nobody else's business.
func (b *botState) status(chat string) string {
	var t strings.Builder
	fmt.Fprintf(&t, "📊 <b>wallapop</b> <code>%s</code>\n", buildVersion)

	if res, ok := watch.LoadResult(b.cfg.DataDir); ok {
		fmt.Fprintf(&t, "\nUltima ronda a las %s", res.StartedAt.Format("15:04"))
		if b.owner(chat) {
			fmt.Fprintf(&t, ": %d usuarios, %d busquedas, %d anuncios, <b>%d nuevos</b>",
				res.Users, res.Watched, res.Scanned, len(res.New))
		}
		t.WriteString("\n")
	}
	if next := b.nextWatch(); !next.IsZero() {
		fmt.Fprintf(&t, "<i>proxima a las %s</i>\n", next.Format("15:04"))
	}
	if user, ok := b.people.Get(chat); ok {
		fmt.Fprintf(&t, "Tus busquedas: %d de %d\n", len(user.Searches), b.cfg.MaxSearches)
	}
	if !b.owner(chat) {
		return strings.TrimRight(t.String(), "\n")
	}

	waiting := 0
	all := b.people.All()
	for _, user := range all {
		if !user.Active {
			waiting++
		}
	}
	fmt.Fprintf(&t, "\n👥 <b>%d usuarios</b>", len(all)-waiting)
	if waiting > 0 {
		fmt.Fprintf(&t, " · %d esperando", waiting)
	}
	t.WriteString("\n")

	if res, ok := reactivate.LoadResult(b.cfg.DataDir); ok {
		fmt.Fprintf(&t, "\n♻️ <b>Catalogo</b> · %s\n", res.StartedAt.Format("02/01"))
		fmt.Fprintf(&t, "%d anuncios · %d caducados · <b>%d reactivados</b>\n",
			res.Catalogue, res.Expired, len(res.Reactivated))
	}
	if next := b.nextRun(); !next.IsZero() {
		fmt.Fprintf(&t, "<i>proxima pasada el %s</i>\n", next.Format("02/01 a las 15:04"))
	}
	if sess := b.store.Current(); sess != nil {
		if left, ok := sess.Renewable(); ok {
			fmt.Fprintf(&t, "\n🔑 <b>Sesion</b> · %.0f dias de margen", left.Hours()/24)
		}
	}
	return strings.TrimRight(t.String(), "\n")
}
