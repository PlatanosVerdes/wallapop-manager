package main

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"unicode"
)

var greetings = map[string]bool{
	"hola": true, "holi": true, "holaa": true, "buenas": true, "hey": true, "ey": true, "hi": true, "hello": true,
	"gracias": true, "muchas gracias": true, "grax": true, "thx": true, "thanks": true, "merci": true,
	"ok": true, "okey": true, "vale": true, "perfecto": true, "genial": true, "guay": true, "top": true, "bien": true,
	"si": true, "sí": true, "no": true, "claro": true, "venga": true, "dale": true,
	"adios": true, "adiós": true, "chao": true, "chau": true, "bye": true, "hasta luego": true,
	"buenos dias": true, "buenos días": true, "buenas tardes": true, "buenas noches": true,
	"que tal": true, "qué tal": true, "como estas": true, "cómo estás": true, "que haces": true, "qué haces": true,
	"lol": true, "xd": true, "ah": true, "oh": true, "vaya": true, "wow": true, "ostras": true,
}

var laughter = regexp.MustCompile(`^(ja|je|ji|jo|ha|he|ke|xd)+[ajhexd]*$`)

func smallTalk(text string) bool {
	plain := strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
	return plain == "" || greetings[plain] || laughter.MatchString(strings.ReplaceAll(plain, " ", ""))
}

var jokes = []string{
	"🤖 Yo de charla voy justito, lo mío es buscar chollos.",
	"🧐 Eso no lo encuentro ni en Wallapop.",
	"😅 Me pillas regateando con un señor por un Kallax.",
	"🦜 Soy un loro con una sola frase: dime qué busco.",
	"🛵 Mientras hablamos alguien está vendiendo tu moto soñada. ¿Qué busco?",
	"🙌 ¡Igualmente! Y ahora, ¿qué te vigilo?",
	"🕵️ Estoy de guardia. Dame algo que vigilar.",
}

func joke() string {
	return jokes[rand.IntN(len(jokes))] + "\n\n<i>Escríbeme lo que buscas, por ejemplo: bici 100-300 en Sant Cugat</i>"
}
